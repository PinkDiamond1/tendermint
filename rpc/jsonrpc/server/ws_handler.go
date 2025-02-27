package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/rpc/client"
	"github.com/tendermint/tendermint/rpc/coretypes"
	rpctypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
)

// WebSocket handler

const (
	defaultWSWriteChanCapacity = 100
	defaultWSWriteWait         = 10 * time.Second
	defaultWSReadWait          = 30 * time.Second
	defaultWSPingPeriod        = (defaultWSReadWait * 9) / 10
)

// WebsocketManager provides a WS handler for incoming connections and passes a
// map of functions along with any additional params to new connections.
// NOTE: The websocket path is defined externally, e.g. in node/node.go
type WebsocketManager struct {
	websocket.Upgrader

	funcMap       map[string]*RPCFunc
	logger        log.Logger
	wsConnOptions []func(*wsConnection)
}

// NewWebsocketManager returns a new WebsocketManager that passes a map of
// functions, connection options and logger to new WS connections.
func NewWebsocketManager(
	funcMap map[string]*RPCFunc,
	wsConnOptions ...func(*wsConnection),
) *WebsocketManager {
	return &WebsocketManager{
		funcMap: funcMap,
		Upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// TODO ???
				//
				// The default behavior would be relevant to browser-based clients,
				// afaik. I suppose having a pass-through is a workaround for allowing
				// for more complex security schemes, shifting the burden of
				// AuthN/AuthZ outside the Tendermint RPC.
				// I can't think of other uses right now that would warrant a TODO
				// though. The real backstory of this TODO shall remain shrouded in
				// mystery
				return true
			},
		},
		logger:        log.NewNopLogger(),
		wsConnOptions: wsConnOptions,
	}
}

// SetLogger sets the logger.
func (wm *WebsocketManager) SetLogger(l log.Logger) {
	wm.logger = l
}

// WebsocketHandler upgrades the request/response (via http.Hijack) and starts
// the wsConnection.
func (wm *WebsocketManager) WebsocketHandler(w http.ResponseWriter, r *http.Request) {
	wsConn, err := wm.Upgrade(w, r, nil)
	if err != nil {
		// TODO - return http error
		wm.logger.Error("Failed to upgrade connection", "err", err)
		return
	}
	defer func() {
		if err := wsConn.Close(); err != nil {
			wm.logger.Error("Failed to close connection", "err", err)
		}
	}()

	// register connection
	logger := wm.logger.With("remote", wsConn.RemoteAddr())
	conn := newWSConnection(wsConn, wm.funcMap, logger, wm.wsConnOptions...)
	wm.logger.Info("New websocket connection", "remote", conn.remoteAddr)

	// starting the conn is blocking
	if err = conn.Start(r.Context()); err != nil {
		wm.logger.Error("Failed to start connection", "err", err)
		return
	}

	if err := conn.Stop(); err != nil {
		wm.logger.Error("error while stopping connection", "error", err)
	}
}

// WebSocket connection

// A single websocket connection contains listener id, underlying ws
// connection, and the event switch for subscribing to events.
//
// In case of an error, the connection is stopped.
type wsConnection struct {
	*client.RunState

	remoteAddr string
	baseConn   *websocket.Conn
	// writeChan is never closed, to allow WriteRPCResponse() to fail.
	writeChan chan rpctypes.RPCResponse

	// chan, which is closed when/if readRoutine errors
	// used to abort writeRoutine
	readRoutineQuit chan struct{}

	funcMap map[string]*RPCFunc

	// write channel capacity
	writeChanCapacity int

	// each write times out after this.
	writeWait time.Duration

	// Connection times out if we haven't received *anything* in this long, not even pings.
	readWait time.Duration

	// Send pings to server with this period. Must be less than readWait, but greater than zero.
	pingPeriod time.Duration

	// Maximum message size.
	readLimit int64

	// callback which is called upon disconnect
	onDisconnect func(remoteAddr string)

	ctx    context.Context
	cancel context.CancelFunc
}

// NewWSConnection wraps websocket.Conn.
//
// See the commentary on the func(*wsConnection) functions for a detailed
// description of how to configure ping period and pong wait time. NOTE: if the
// write buffer is full, pongs may be dropped, which may cause clients to
// disconnect. see https://github.com/gorilla/websocket/issues/97
func newWSConnection(
	baseConn *websocket.Conn,
	funcMap map[string]*RPCFunc,
	logger log.Logger,
	options ...func(*wsConnection),
) *wsConnection {
	wsc := &wsConnection{
		RunState:          client.NewRunState("wsConnection", logger),
		remoteAddr:        baseConn.RemoteAddr().String(),
		baseConn:          baseConn,
		funcMap:           funcMap,
		writeWait:         defaultWSWriteWait,
		writeChanCapacity: defaultWSWriteChanCapacity,
		readWait:          defaultWSReadWait,
		pingPeriod:        defaultWSPingPeriod,
		readRoutineQuit:   make(chan struct{}),
	}
	for _, option := range options {
		option(wsc)
	}
	wsc.baseConn.SetReadLimit(wsc.readLimit)
	return wsc
}

// OnDisconnect sets a callback which is used upon disconnect - not
// Goroutine-safe. Nop by default.
func OnDisconnect(onDisconnect func(remoteAddr string)) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.onDisconnect = onDisconnect
	}
}

// WriteWait sets the amount of time to wait before a websocket write times out.
// It should only be used in the constructor - not Goroutine-safe.
func WriteWait(writeWait time.Duration) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.writeWait = writeWait
	}
}

// WriteChanCapacity sets the capacity of the websocket write channel.
// It should only be used in the constructor - not Goroutine-safe.
func WriteChanCapacity(cap int) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.writeChanCapacity = cap
	}
}

// ReadWait sets the amount of time to wait before a websocket read times out.
// It should only be used in the constructor - not Goroutine-safe.
func ReadWait(readWait time.Duration) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.readWait = readWait
	}
}

// PingPeriod sets the duration for sending websocket pings.
// It should only be used in the constructor - not Goroutine-safe.
func PingPeriod(pingPeriod time.Duration) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.pingPeriod = pingPeriod
	}
}

// ReadLimit sets the maximum size for reading message.
// It should only be used in the constructor - not Goroutine-safe.
func ReadLimit(readLimit int64) func(*wsConnection) {
	return func(wsc *wsConnection) {
		wsc.readLimit = readLimit
	}
}

// Start starts the client service routines and blocks until there is an error.
func (wsc *wsConnection) Start(ctx context.Context) error {
	if err := wsc.RunState.Start(ctx); err != nil {
		return err
	}
	wsc.writeChan = make(chan rpctypes.RPCResponse, wsc.writeChanCapacity)

	// Read subscriptions/unsubscriptions to events
	go wsc.readRoutine(ctx)
	// Write responses, BLOCKING.
	wsc.writeRoutine(ctx)

	return nil
}

// Stop unsubscribes the remote from all subscriptions.
func (wsc *wsConnection) Stop() error {
	if err := wsc.RunState.Stop(); err != nil {
		return err
	}
	if wsc.onDisconnect != nil {
		wsc.onDisconnect(wsc.remoteAddr)
	}
	if wsc.ctx != nil {
		wsc.cancel()
	}
	return nil
}

// GetRemoteAddr returns the remote address of the underlying connection.
// It implements WSRPCConnection
func (wsc *wsConnection) GetRemoteAddr() string {
	return wsc.remoteAddr
}

// WriteRPCResponse pushes a response to the writeChan, and blocks until it is
// accepted.
// It implements WSRPCConnection. It is Goroutine-safe.
func (wsc *wsConnection) WriteRPCResponse(ctx context.Context, resp rpctypes.RPCResponse) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case wsc.writeChan <- resp:
		return nil
	}
}

// TryWriteRPCResponse attempts to push a response to the writeChan, but does
// not block.
// It implements WSRPCConnection. It is Goroutine-safe
func (wsc *wsConnection) TryWriteRPCResponse(ctx context.Context, resp rpctypes.RPCResponse) bool {
	select {
	case <-ctx.Done():
		return false
	case wsc.writeChan <- resp:
		return true
	default:
		return false
	}
}

// Context returns the connection's context.
// The context is canceled when the client's connection closes.
func (wsc *wsConnection) Context() context.Context {
	if wsc.ctx != nil {
		return wsc.ctx
	}
	wsc.ctx, wsc.cancel = context.WithCancel(context.Background())
	return wsc.ctx
}

// Read from the socket and subscribe to or unsubscribe from events
func (wsc *wsConnection) readRoutine(ctx context.Context) {
	// readRoutine will block until response is written or WS connection is closed
	writeCtx := context.Background()

	defer func() {
		if r := recover(); r != nil {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("WSJSONRPC: %v", r)
			}
			wsc.Logger.Error("Panic in WSJSONRPC handler", "err", err, "stack", string(debug.Stack()))
			if err := wsc.WriteRPCResponse(writeCtx, rpctypes.RPCInternalError(rpctypes.JSONRPCIntID(-1), err)); err != nil {
				wsc.Logger.Error("error writing RPC response", "err", err)
			}
			go wsc.readRoutine(ctx)
		}
	}()

	wsc.baseConn.SetPongHandler(func(m string) error {
		return wsc.baseConn.SetReadDeadline(time.Now().Add(wsc.readWait))
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
			// reset deadline for every type of message (control or data)
			if err := wsc.baseConn.SetReadDeadline(time.Now().Add(wsc.readWait)); err != nil {
				wsc.Logger.Error("failed to set read deadline", "err", err)
			}

			_, r, err := wsc.baseConn.NextReader()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					wsc.Logger.Info("Client closed the connection")
				} else {
					wsc.Logger.Error("Failed to read request", "err", err)
				}
				if err := wsc.Stop(); err != nil {
					wsc.Logger.Error("error closing websocket connection", "err", err)
				}
				close(wsc.readRoutineQuit)
				return
			}

			dec := json.NewDecoder(r)
			var request rpctypes.RPCRequest
			err = dec.Decode(&request)
			if err != nil {
				if err := wsc.WriteRPCResponse(writeCtx,
					rpctypes.RPCParseError(fmt.Errorf("error unmarshaling request: %w", err))); err != nil {
					wsc.Logger.Error("error writing RPC response", "err", err)
				}
				continue
			}

			// A Notification is a Request object without an "id" member.
			// The Server MUST NOT reply to a Notification, including those that are within a batch request.
			if request.ID == nil {
				wsc.Logger.Debug(
					"WSJSONRPC received a notification, skipping... (please send a non-empty ID if you want to call a method)",
					"req", request,
				)
				continue
			}

			// Now, fetch the RPCFunc and execute it.
			rpcFunc := wsc.funcMap[request.Method]
			if rpcFunc == nil {
				if err := wsc.WriteRPCResponse(writeCtx, rpctypes.RPCMethodNotFoundError(request.ID)); err != nil {
					wsc.Logger.Error("error writing RPC response", "err", err)
				}
				continue
			}

			ctx := &rpctypes.Context{JSONReq: &request, WSConn: wsc}
			args := []reflect.Value{reflect.ValueOf(ctx)}
			if len(request.Params) > 0 {
				fnArgs, err := jsonParamsToArgs(rpcFunc, request.Params)
				if err != nil {
					if err := wsc.WriteRPCResponse(writeCtx,
						rpctypes.RPCInvalidParamsError(request.ID, fmt.Errorf("error converting json params to arguments: %w", err)),
					); err != nil {
						wsc.Logger.Error("error writing RPC response", "err", err)
					}
					continue
				}
				args = append(args, fnArgs...)
			}

			returns := rpcFunc.f.Call(args)

			// TODO: Need to encode args/returns to string if we want to log them
			wsc.Logger.Info("WSJSONRPC", "method", request.Method)

			var resp rpctypes.RPCResponse
			result, err := unreflectResult(returns)
			switch e := err.(type) {
			// if no error then return a success response
			case nil:
				resp = rpctypes.NewRPCSuccessResponse(request.ID, result)

			// if this already of type RPC error then forward that error
			case *rpctypes.RPCError:
				resp = rpctypes.NewRPCErrorResponse(request.ID, e.Code, e.Message, e.Data)

			default: // we need to unwrap the error and parse it accordingly
				switch errors.Unwrap(err) {
				// check if the error was due to an invald request
				case coretypes.ErrZeroOrNegativeHeight, coretypes.ErrZeroOrNegativePerPage,
					coretypes.ErrPageOutOfRange, coretypes.ErrInvalidRequest:
					resp = rpctypes.RPCInvalidRequestError(request.ID, err)

				// lastly default all remaining errors as internal errors
				default: // includes ctypes.ErrHeightNotAvailable and ctypes.ErrHeightExceedsChainHead
					resp = rpctypes.RPCInternalError(request.ID, err)
				}
			}

			if err := wsc.WriteRPCResponse(writeCtx, resp); err != nil {
				wsc.Logger.Error("error writing RPC response", "err", err)
			}

		}
	}
}

// receives on a write channel and writes out on the socket
func (wsc *wsConnection) writeRoutine(ctx context.Context) {
	pingTicker := time.NewTicker(wsc.pingPeriod)
	defer pingTicker.Stop()

	// https://github.com/gorilla/websocket/issues/97
	pongs := make(chan string, 1)
	wsc.baseConn.SetPingHandler(func(m string) error {
		select {
		case pongs <- m:
		default:
		}
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		case <-wsc.readRoutineQuit: // error in readRoutine
			return
		case m := <-pongs:
			err := wsc.writeMessageWithDeadline(websocket.PongMessage, []byte(m))
			if err != nil {
				wsc.Logger.Info("Failed to write pong (client may disconnect)", "err", err)
			}
		case <-pingTicker.C:
			err := wsc.writeMessageWithDeadline(websocket.PingMessage, []byte{})
			if err != nil {
				wsc.Logger.Error("Failed to write ping", "err", err)
				return
			}
		case msg := <-wsc.writeChan:
			jsonBytes, err := json.MarshalIndent(msg, "", "  ")
			if err != nil {
				wsc.Logger.Error("Failed to marshal RPCResponse to JSON", "err", err)
				continue
			}
			if err = wsc.writeMessageWithDeadline(websocket.TextMessage, jsonBytes); err != nil {
				wsc.Logger.Error("Failed to write response", "err", err, "msg", msg)
				return
			}
		}
	}
}

// All writes to the websocket must (re)set the write deadline.
// If some writes don't set it while others do, they may timeout incorrectly
// (https://github.com/tendermint/tendermint/issues/553)
func (wsc *wsConnection) writeMessageWithDeadline(msgType int, msg []byte) error {
	if err := wsc.baseConn.SetWriteDeadline(time.Now().Add(wsc.writeWait)); err != nil {
		return err
	}
	return wsc.baseConn.WriteMessage(msgType, msg)
}
