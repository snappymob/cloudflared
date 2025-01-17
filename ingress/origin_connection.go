package ingress

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"

	gws "github.com/gorilla/websocket"
	"github.com/rs/zerolog"

	"github.com/cloudflare/cloudflared/ipaccess"
	"github.com/cloudflare/cloudflared/socks"
	"github.com/cloudflare/cloudflared/websocket"
)

// OriginConnection is a way to stream to a service running on the user's origin.
// Different concrete implementations will stream different protocols as long as they are io.ReadWriters.
type OriginConnection interface {
	// Stream should generally be implemented as a bidirectional io.Copy.
	Stream(ctx context.Context, tunnelConn io.ReadWriter, log *zerolog.Logger)
	Close()
}

type streamHandlerFunc func(originConn io.ReadWriter, remoteConn net.Conn, log *zerolog.Logger)

// Stream copies copy data to & from provided io.ReadWriters.
func Stream(conn, backendConn io.ReadWriter, log *zerolog.Logger) {
	proxyDone := make(chan struct{}, 2)

	go func() {
		_, err := io.Copy(conn, backendConn)
		if err != nil {
			log.Debug().Msgf("conn to backendConn copy: %v", err)
		}
		proxyDone <- struct{}{}
	}()

	go func() {
		_, err := io.Copy(backendConn, conn)
		if err != nil {
			log.Debug().Msgf("backendConn to conn copy: %v", err)
		}
		proxyDone <- struct{}{}
	}()

	// If one side is done, we are done.
	<-proxyDone
}

// DefaultStreamHandler is an implementation of streamHandlerFunc that
// performs a two way io.Copy between originConn and remoteConn.
func DefaultStreamHandler(originConn io.ReadWriter, remoteConn net.Conn, log *zerolog.Logger) {
	Stream(originConn, remoteConn, log)
}

// tcpConnection is an OriginConnection that directly streams to raw TCP.
type tcpConnection struct {
	conn net.Conn
}

func (tc *tcpConnection) Stream(ctx context.Context, tunnelConn io.ReadWriter, log *zerolog.Logger) {
	Stream(tunnelConn, tc.conn, log)
}

func (tc *tcpConnection) Close() {
	tc.conn.Close()
}

// tcpOverWSConnection is an OriginConnection that streams to TCP over WS.
type tcpOverWSConnection struct {
	conn          net.Conn
	streamHandler streamHandlerFunc
}

func (wc *tcpOverWSConnection) Stream(ctx context.Context, tunnelConn io.ReadWriter, log *zerolog.Logger) {
	wc.streamHandler(websocket.NewConn(ctx, tunnelConn, log), wc.conn, log)
}

func (wc *tcpOverWSConnection) Close() {
	wc.conn.Close()
}

// wsConnection is an OriginConnection that streams WS between eyeball and origin.
type wsConnection struct {
	wsConn *gws.Conn
	resp   *http.Response
}

func (wsc *wsConnection) Stream(ctx context.Context, tunnelConn io.ReadWriter, log *zerolog.Logger) {
	Stream(tunnelConn, wsc.wsConn.UnderlyingConn(), log)
}

func (wsc *wsConnection) Close() {
	wsc.resp.Body.Close()
	wsc.wsConn.Close()
}

func newWSConnection(clientTLSConfig *tls.Config, r *http.Request) (OriginConnection, *http.Response, error) {
	d := &gws.Dialer{
		TLSClientConfig: clientTLSConfig,
	}
	wsConn, resp, err := websocket.ClientConnect(r, d)
	if err != nil {
		return nil, nil, err
	}
	return &wsConnection{
		wsConn,
		resp,
	}, resp, nil
}

// socksProxyOverWSConnection is an OriginConnection that streams SOCKS connections over WS.
// The connection to the origin happens inside the SOCKS code as the client specifies the origin
// details in the packet.
type socksProxyOverWSConnection struct {
	accessPolicy *ipaccess.Policy
}

func (sp *socksProxyOverWSConnection) Stream(ctx context.Context, tunnelConn io.ReadWriter, log *zerolog.Logger) {
	socks.StreamNetHandler(websocket.NewConn(ctx, tunnelConn, log), sp.accessPolicy, log)
}

func (sp *socksProxyOverWSConnection) Close() {
}
