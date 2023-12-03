package http

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/metacubex/mihomo/adapter/inbound"
	"github.com/metacubex/mihomo/common/lru"
	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"
	authStore "github.com/metacubex/mihomo/listener/auth"
	"github.com/metacubex/mihomo/log"
)

func HandleConn(c net.Conn, tunnel C.Tunnel, cache *lru.LruCache[string, bool], additions ...inbound.Addition) {
	client := newClient(c, tunnel, additions...)
	defer client.CloseIdleConnections()

	conn := N.NewBufferedConn(c)

	keepAlive := true
	trusted := cache == nil // disable authenticate if lru is nil

	for keepAlive {
		request, err := ReadRequest(conn.Reader())
		if err != nil {
			break
		}

		request.RemoteAddr = conn.RemoteAddr().String()

		keepAlive = strings.TrimSpace(strings.ToLower(request.Header.Get("Proxy-Connection"))) == "keep-alive"

		var resp *http.Response

		if !trusted {
			resp = Authenticate(request, cache)

			trusted = resp == nil
		}

		if trusted {
			if request.Method == http.MethodConnect {
				// Manual writing to support CONNECT for http 1.0 (workaround for uplay client)
				if _, err = fmt.Fprintf(conn, "HTTP/%d.%d %03d %s\r\n\r\n", request.ProtoMajor, request.ProtoMinor, http.StatusOK, "Connection established"); err != nil {
					break // close connection
				}

				tunnel.HandleTCPConn(inbound.NewHTTPS(request, conn, additions...))

				return // hijack connection
			}

			host := request.Header.Get("Host")
			if host != "" {
				request.Host = host
			}

			request.RequestURI = ""

			if isUpgradeRequest(request) {
				handleUpgrade(conn, request, tunnel, additions...)

				return // hijack connection
			}

			RemoveHopByHopHeaders(request.Header)
			RemoveExtraHTTPHostPort(request)

			if request.URL.Scheme == "" || request.URL.Host == "" {
				resp = ResponseWith(request, http.StatusBadRequest)
			} else {
				resp, err = client.Do(request)
				if err != nil {
					resp = ResponseWith(request, http.StatusBadGateway)
				}
			}

			RemoveHopByHopHeaders(resp.Header)
		}

		if keepAlive {
			resp.Header.Set("Proxy-Connection", "keep-alive")
			resp.Header.Set("Connection", "keep-alive")
			resp.Header.Set("Keep-Alive", "timeout=4")
		}

		resp.Close = !keepAlive

		err = resp.Write(conn)
		if err != nil {
			break // close connection
		}
	}

	_ = conn.Close()
}

func Authenticate(request *http.Request, cache *lru.LruCache[string, bool]) *http.Response {
	authenticator := authStore.Authenticator()
	if inbound.SkipAuthRemoteAddress(request.RemoteAddr) {
		authenticator = nil
	}
	if authenticator != nil {
		credential := parseBasicProxyAuthorization(request)
		if credential == "" {
			resp := ResponseWith(request, http.StatusProxyAuthRequired)
			resp.Header.Set("Proxy-Authenticate", "Basic")
			return resp
		}

		authed, exist := cache.Get(credential)
		if !exist {
			user, pass, err := decodeBasicProxyAuthorization(credential)
			authed = err == nil && authenticator.Verify(user, pass)
			cache.Set(credential, authed)
		}
		if !authed {
			log.Infoln("Auth failed from %s", request.RemoteAddr)

			return ResponseWith(request, http.StatusForbidden)
		}
	}

	return nil
}

func ResponseWith(request *http.Request, statusCode int) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Proto:      request.Proto,
		ProtoMajor: request.ProtoMajor,
		ProtoMinor: request.ProtoMinor,
		Header:     http.Header{},
	}
}
