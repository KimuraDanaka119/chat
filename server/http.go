/******************************************************************************
 *
 *  Description :
 *
 *  Web server initialization and shutdown.
 *
 *****************************************************************************/

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tinode/chat/server/store"
	"github.com/tinode/chat/server/store/types"

	"golang.org/x/crypto/acme/autocert"
)

type tlsConfig struct {
	// Flag enabling TLS
	Enabled bool `json:"enabled"`
	// Listen on port 80 and redirect plain HTTP to HTTPS
	RedirectHTTP string `json:"http_redirect"`
	// Enable Strict-Transport-Security by setting max_age > 0
	StrictMaxAge int `json:"strict_max_age"`
	// ACME autocert config, e.g. letsencrypt.org
	Autocert *tlsAutocertConfig `json:"autocert"`
	// If Autocert is not defined, provide file names of static certificate and key
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type tlsAutocertConfig struct {
	// Domains to support by autocert
	Domains []string `json:"domains"`
	// Name of directory where auto-certificates are cached, e.g. /etc/letsencrypt/live/your-domain-here
	CertCache string `json:"cache"`
	// Contact email for letsencrypt
	Email string `json:"email"`
}

func getTLSConfig(config *tlsConfig) (*tls.Config, error) {
	// If autocert is provided, use it.
	if config.Autocert != nil {
		certManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(config.Autocert.Domains...),
			Cache:      autocert.DirCache(config.Autocert.CertCache),
			Email:      config.Autocert.Email,
		}
		return &tls.Config{GetCertificate: certManager.GetCertificate}, nil
	}

	// Otherwise try to use static keys.
	cert, err := tls.LoadX509KeyPair(config.CertFile, config.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func listenAndServe(addr string, mux *http.ServeMux, tlsEnabled bool, jsconfig string, stop <-chan bool) error {
	var tlsConfig tlsConfig

	if jsconfig != "" {
		if err := json.Unmarshal([]byte(jsconfig), &tlsConfig); err != nil {
			return errors.New("http: failed to parse tls_config: " + err.Error() + "(" + jsconfig + ")")
		}
	}

	shuttingDown := false

	httpdone := make(chan bool)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if tlsEnabled || tlsConfig.Enabled {

		if tlsConfig.StrictMaxAge > 0 {
			globals.tlsStrictMaxAge = strconv.Itoa(tlsConfig.StrictMaxAge)
		}

		// Configure TLS certificate, if necessary.
		var err error
		if server.TLSConfig, err = getTLSConfig(&tlsConfig); err != nil {
			return err
		}
	}

	go func() {
		var err error
		if server.TLSConfig != nil {
			// If port is not specified, use default https port (443),
			// otherwise it will default to 80
			if server.Addr == "" {
				server.Addr = ":https"
			}

			if tlsConfig.RedirectHTTP != "" {
				log.Printf("Redirecting connections from HTTP at [%s] to HTTPS at [%s]",
					tlsConfig.RedirectHTTP, server.Addr)

				// This is a second HTTP server listenning on a different port.
				go http.ListenAndServe(tlsConfig.RedirectHTTP, tlsRedirect(addr))
			}

			log.Printf("Listening for client HTTPS connections on [%s]", server.Addr)
			err = server.ListenAndServeTLS("", "")
		} else {
			log.Printf("Listening for client HTTP connections on [%s]", server.Addr)
			err = server.ListenAndServe()
		}
		if err != nil {
			if shuttingDown {
				log.Println("HTTP server: stopped")
			} else {
				log.Println("HTTP server: failed", err)
			}
		}
		httpdone <- true
	}()

	// Wait for either a termination signal or an error
loop:
	for {
		select {
		case <-stop:
			// Flip the flag that we are terminating and close the Accept-ing socket, so no new connections are possible.
			shuttingDown = true
			// Give server 2 seconds to shut down.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := server.Shutdown(ctx); err != nil {
				// failure/timeout shutting down the server gracefully
				log.Println("HTTP server failed to terminate gracefully", err)
			}

			// While the server shuts down, termianate all sessions.
			globals.sessionStore.Shutdown()

			// Wait for http server to stop Accept()-ing connections
			<-httpdone
			cancel()

			// Shutdown local cluster node, if it's a part of a cluster.
			globals.cluster.shutdown()

			// Terminate plugin connections
			pluginsShutdown()

			// Shutdown gRPC server, if one is configured.
			if globals.grpcServer != nil {
				// GracefulStop does not terminate ServerStream. Must use Stop().
				globals.grpcServer.Stop()
			}

			// Shutdown the hub. The hub will shutdown topics
			hubdone := make(chan bool)
			globals.hub.shutdown <- hubdone

			// wait for the hub to finish
			<-hubdone

			break loop

		case <-httpdone:
			break loop
		}
	}
	return nil
}

func signalHandler() <-chan bool {
	stop := make(chan bool)

	signchan := make(chan os.Signal, 1)
	signal.Notify(signchan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		// Wait for a signal. Don't care which signal it is
		sig := <-signchan
		log.Printf("Signal received: '%s', shutting down", sig)
		stop <- true
	}()

	return stop
}

// Wrapper for http.Handler which optionally adds a Strict-Transport-Security to the response
func hstsHandler(handler http.Handler) http.Handler {
	if globals.tlsStrictMaxAge != "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Strict-Transport-Security", "max-age="+globals.tlsStrictMaxAge)
			handler.ServeHTTP(w, r)
		})
	}
	return handler
}

// The following code is used to intercept HTTP errors so they can be wrapped into json.

// Wrapper around http.ResponseWriter which detects status set to 400+ and replaces
// default error message with a custom one.
type errorResponseWriter struct {
	status int
	http.ResponseWriter
}

func (w *errorResponseWriter) WriteHeader(status int) {
	if status >= http.StatusBadRequest {
		// charset=utf-8 is the default. No need to write it explicitly
		// Must set all the headers before calling super.WriteHeader()
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *errorResponseWriter) Write(p []byte) (n int, err error) {
	if w.status >= http.StatusBadRequest {
		p, _ = json.Marshal(
			&ServerComMessage{Ctrl: &MsgServerCtrl{
				Timestamp: time.Now().UTC().Round(time.Millisecond),
				Code:      w.status,
				Text:      http.StatusText(w.status)}})
	}
	return w.ResponseWriter.Write(p)
}

// Handler which deploys errorResponseWriter
func httpErrorHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(&errorResponseWriter{0, w}, r)
		})
}

// Custom 404 response.
func serve404(wrt http.ResponseWriter, req *http.Request) {
	wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
	wrt.WriteHeader(http.StatusNotFound)
	json.NewEncoder(wrt).Encode(
		&ServerComMessage{Ctrl: &MsgServerCtrl{
			Timestamp: time.Now().UTC().Round(time.Millisecond),
			Code:      http.StatusNotFound,
			Text:      "not found"}})
}

// Redirect HTTP requests to HTTPS
func tlsRedirect(toPort string) http.HandlerFunc {
	if toPort == ":443" || toPort == ":https" {
		toPort = ""
	} else if toPort != "" && toPort[:1] == ":" {
		// Strip leading colon. JoinHostPort will add it back.
		toPort = toPort[1:]
	}

	return func(wrt http.ResponseWriter, req *http.Request) {
		target := *req.URL
		target.Scheme = "https"
		// Host name is guaranteed to be valid because of TLS whitelist.
		host, _, _ := net.SplitHostPort(req.Host)
		if target.Port() != "" {
			if toPort != "" {
				// Replace the port number.
				target.Host = net.JoinHostPort(host, toPort)
			} else {
				// Just strip the port number.
				target.Host = host
			}
		}
		http.Redirect(wrt, req, target.String(), http.StatusTemporaryRedirect)
	}
}

// Wrapper for http.Handler which optionally adds a Cache-Control header to the response
func cacheControlHandler(maxAge int, handler http.Handler) http.Handler {
	if maxAge > 0 {
		strMaxAge := strconv.Itoa(maxAge)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "must-revalidate, public, max-age="+strMaxAge)
			handler.ServeHTTP(w, r)
		})
	}
	return handler
}

// Get API key from an HTTP request.
func getAPIKey(req *http.Request) string {
	// Check header.
	apikey := req.Header.Get("X-Tinode-APIKey")

	// Check URL query parameters.
	if apikey == "" {
		apikey = req.URL.Query().Get("apikey")
	}

	// Check form values.
	if apikey == "" {
		apikey = req.FormValue("apikey")
	}

	// Check cookies.
	if apikey == "" {
		if c, err := req.Cookie("apikey"); err == nil {
			apikey = c.Value
		}
	}
	return apikey
}

// Extracts authorization credentials from an HTTP request.
// Returns authentication method and secret.
func getHttpAuth(req *http.Request) (method, secret string) {
	// Check X-Tinode-Auth header.
	if parts := strings.Split(req.Header.Get("X-Tinode-Auth"), " "); len(parts) == 2 {
		method, secret = parts[0], parts[1]
		return
	}

	// Check canonical Authorization header.
	if parts := strings.Split(req.Header.Get("Authorization"), " "); len(parts) == 2 {
		method, secret = parts[0], parts[1]
		return
	}

	// Check URL query parameters.
	if method = req.URL.Query().Get("auth"); method != "" {
		secret = req.URL.Query().Get("secret")
		return
	}

	// Check form values.
	if method = req.FormValue("auth"); method != "" {
		return method, req.FormValue("secret")
	}

	// Check cookies as the last resort.
	if mcookie, err := req.Cookie("auth"); err == nil {
		if scookie, err := req.Cookie("secret"); err == nil {
			method, secret = mcookie.Value, scookie.Value
		}
	}

	return
}

// Authenticate non-websocket HTTP request
func authHttpRequest(req *http.Request) (types.Uid, []byte, error) {
	var uid types.Uid
	if authMethod, secret := getHttpAuth(req); authMethod != "" {
		decodedSecret := make([]byte, base64.StdEncoding.DecodedLen(len(secret)))
		if _, err := base64.StdEncoding.Decode(decodedSecret, []byte(secret)); err != nil {
			return uid, nil, types.ErrMalformed
		}
		if authhdl := store.GetLogicalAuthHandler(authMethod); authhdl != nil {
			rec, challenge, err := authhdl.Authenticate(decodedSecret)
			if err != nil {
				return uid, nil, err
			}
			if challenge != nil {
				return uid, challenge, nil
			}
			uid = rec.Uid
		} else {
			log.Println("fileUpload: auth data is present but handler is not found", authMethod)
		}
	} else {
		// Find the session, make sure it's appropriately authenticated.
		sess := globals.sessionStore.Get(req.FormValue("sid"))
		if sess != nil {
			uid = sess.uid
		}
	}
	return uid, nil, nil
}
