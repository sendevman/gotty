package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/yudai/gotty/webtty"
)

func (server *Server) generateHandleWS(ctx context.Context, cancel context.CancelFunc, connections *int64, wg *sync.WaitGroup) http.HandlerFunc {
	once := new(int64)

	timer := time.NewTimer(time.Duration(server.options.Timeout) * time.Second)
	if server.options.Timeout > 0 {
		go func() {
			select {
			case <-timer.C:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if server.options.Once {
			success := atomic.CompareAndSwapInt64(once, 0, 1)
			if !success {
				http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
				return
			}
		}

		if server.options.Timeout > 0 {
			timer.Stop()
		}
		wg.Add(1)
		num := atomic.AddInt64(connections, 1)
		closeReason := "unknown reason"

		defer func() {
			num := atomic.AddInt64(connections, -1)
			if num == 0 && server.options.Timeout > 0 {
				timer.Reset(time.Duration(server.options.Timeout) * time.Second)
			}

			log.Printf(
				"Connection closed by %s: %s, connections: %d/%d",
				closeReason, r.RemoteAddr, num, server.options.MaxConnection,
			)

			if server.options.Once {
				cancel()
			}

			wg.Done()
		}()

		if int64(server.options.MaxConnection) != 0 {
			if num > int64(server.options.MaxConnection) {
				closeReason = "exceeding max number of connections"
				return
			}
		}

		log.Printf("New client connected: %s, connections: %d/%d", r.RemoteAddr, num, server.options.MaxConnection)

		if r.Method != "GET" {
			http.Error(w, "Method not allowed", 405)
			return
		}

		conn, err := server.upgrader.Upgrade(w, r, nil)
		if err != nil {
			http.Error(w, "Failed to upgrade connection: "+err.Error(), 500)
			return
		}
		defer conn.Close()

		err = server.processWSConn(ctx, conn)

		switch err {
		case ctx.Err():
			closeReason = "cancelation"
		case webtty.ErrSlaveClosed:
			closeReason = server.factory.Name()
		case webtty.ErrMasterClosed:
			closeReason = "client"
		default:
			closeReason = fmt.Sprintf("an error: %s", err)
		}
	}
}

func (server *Server) processWSConn(ctx context.Context, conn *websocket.Conn) error {
	typ, initLine, err := conn.ReadMessage()
	if err != nil {
		return errors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if typ != websocket.TextMessage {
		return errors.New("failed to authenticate websocket connection: invalid message type")
	}

	var init InitMessage
	err = json.Unmarshal(initLine, &init)
	if err != nil {
		return errors.Wrapf(err, "failed to authenticate websocket connection")
	}
	if init.AuthToken != server.options.Credential {
		return errors.New("failed to authenticate websocket connection")
	}

	queryPath := "?"
	if server.options.PermitArguments && init.Arguments != "" {
		queryPath = init.Arguments
	}

	query, err := url.Parse(queryPath)
	if err != nil {
		return errors.Wrapf(err, "failed to parse arguments")
	}
	params := query.Query()
	var slave Slave
	slave, err = server.factory.New(params)
	if err != nil {
		return errors.Wrapf(err, "failed to create backend")
	}
	defer slave.Close()

	titleVars := server.titleVariables(
		[]string{"server", "master", "slave"},
		map[string]map[string]interface{}{
			"server": server.options.TitleVariables,
			"master": map[string]interface{}{
				"remote_addr": conn.RemoteAddr(),
			},
			"slave": slave.WindowTitleVariables(),
		},
	)

	titleBuf := new(bytes.Buffer)
	err = server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		return errors.Wrapf(err, "failed to fill window title template")
	}

	opts := []webtty.Option{
		webtty.WithWindowTitle(titleBuf.Bytes()),
	}
	if server.options.PermitWrite {
		opts = append(opts, webtty.WithPermitWrite())
	}
	if server.options.EnableReconnect {
		opts = append(opts, webtty.WithReconnect(server.options.ReconnectTime))
	}
	if server.options.Width > 0 || server.options.Height > 0 {
		width, height, err := slave.GetTerminalSize()
		if err != nil {
			return errors.Wrapf(err, "failed to get default terminal size")
		}
		if server.options.Width > 0 {
			width = server.options.Width
		}
		if server.options.Height > 0 {
			height = server.options.Height
		}
		err = slave.ResizeTerminal(width, height)
		if err != nil {
			return errors.Wrapf(err, "failed to resize terminal")
		}

		opts = append(opts, webtty.WithFixedSize(server.options.Width, server.options.Height))
	}
	if server.options.Preferences != nil {
		opts = append(opts, webtty.WithMasterPreferences(server.options.Preferences))
	}

	tty, err := webtty.New(conn, slave, opts...)
	if err != nil {
		return errors.Wrapf(err, "failed to create webtty")
	}

	err = tty.Run(ctx)

	return err
}

func (server *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	titleVars := server.titleVariables(
		[]string{"server", "master"},
		map[string]map[string]interface{}{
			"server": server.options.TitleVariables,
			"master": map[string]interface{}{
				"remote_addr": r.RemoteAddr,
			},
		},
	)

	titleBuf := new(bytes.Buffer)
	err := server.titleTemplate.Execute(titleBuf, titleVars)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	indexVars := map[string]interface{}{
		"title": titleBuf.String(),
	}

	indexBuf := new(bytes.Buffer)
	err = server.indexTemplate.Execute(indexBuf, indexVars)
	if err != nil {
		http.Error(w, "Internal Server Error", 500)
		return
	}

	w.Write(indexBuf.Bytes())
}

func (server *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	// @TODO hashing?
	w.Write([]byte("var gotty_auth_token = '" + server.options.Credential + "';"))
}

// titleVariables merges maps in a specified order.
// varUnits are name-keyed maps, whose names will be iterated using order.
func (server *Server) titleVariables(order []string, varUnits map[string]map[string]interface{}) map[string]interface{} {
	titleVars := map[string]interface{}{}

	for _, name := range order {
		vars, ok := varUnits[name]
		if !ok {
			panic("title variable name error")
		}
		for key, val := range vars {
			titleVars[key] = val
		}
	}

	// safe net for conflicted keys
	for _, name := range order {
		titleVars[name] = varUnits[name]
	}

	return titleVars
}
