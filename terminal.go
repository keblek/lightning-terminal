package terminal

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	restProxy "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/jessevdk/go-flags"
	"github.com/lightninglabs/faraday/frdrpc"
	"github.com/lightninglabs/lndclient"
	"github.com/lightninglabs/loop"
	"github.com/lightninglabs/loop/loopd"
	"github.com/lightninglabs/loop/looprpc"
	"github.com/lightninglabs/pool"
	"github.com/lightninglabs/pool/poolrpc"
	"github.com/lightningnetwork/lnd"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/signal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	defaultServerTimeout  = 10 * time.Second
	defaultConnectTimeout = 5 * time.Second
	defaultStartupTimeout = 5 * time.Second
)

var (
	// maxMsgRecvSize is the largest message our REST proxy will receive. We
	// set this to 200MiB atm.
	maxMsgRecvSize = grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200)

	// appBuildFS is an in-memory file system that contains all the static
	// HTML/CSS/JS files of the UI. It is compiled into the binary with the
	// go 1.16 embed directive below. Because the path is relative to the
	// root package, all assets will have a path prefix of /app/build/ which
	// we'll strip by giving a sub directory to the HTTP server.
	//
	//go:embed app/build/*
	appBuildFS embed.FS

	// appFilesDir is the sub directory of the above build directory which
	// we pass to the HTTP server.
	appFilesDir = "app/build"
)

// LightningTerminal is the main grand unified binary instance. Its task is to
// start an lnd node then start and register external subservers to it.
type LightningTerminal struct {
	cfg *Config

	wg         sync.WaitGroup
	lndErrChan chan error

	lndClient *lndclient.GrpcLndServices

	faradayServer  *frdrpc.RPCServer
	faradayStarted bool

	loopServer  *loopd.Daemon
	loopStarted bool

	poolServer  *pool.Server
	poolStarted bool

	rpcProxy   *rpcProxy
	httpServer *http.Server
}

// New creates a new instance of the lightning-terminal daemon.
func New() *LightningTerminal {
	return &LightningTerminal{
		lndErrChan: make(chan error, 1),
	}
}

// Run starts everything and then blocks until either the application is shut
// down or a critical error happens.
func (g *LightningTerminal) Run() error {
	cfg, err := loadAndValidateConfig()
	if err != nil {
		return fmt.Errorf("could not load config: %w", err)
	}
	g.cfg = cfg

	// Create the instances of our subservers now so we can hook them up to
	// lnd once it's fully started.
	g.faradayServer = frdrpc.NewRPCServer(g.cfg.faradayRpcConfig)
	g.loopServer = loopd.New(g.cfg.Loop, nil)
	g.poolServer = pool.NewServer(g.cfg.Pool)
	g.rpcProxy = newRpcProxy(g.cfg, g, getAllPermissions())

	// Overwrite the loop and pool daemon's user agent name so it sends
	// "litd" instead of "loopd" and "poold" respectively.
	loop.AgentName = "litd"
	pool.SetAgentName("litd")

	// Hook interceptor for os signals.
	err = signal.Intercept()
	if err != nil {
		return fmt.Errorf("could not intercept signals: %v", err)
	}

	// Call the "real" main in a nested manner so the defers will properly
	// be executed in the case of a graceful shutdown.
	readyChan := make(chan struct{})
	unlockChan := make(chan struct{})
	if g.cfg.LndMode == ModeIntegrated {
		g.wg.Add(1)
		go func() {
			defer g.wg.Done()

			extSubCfg := &lnd.RPCSubserverConfig{
				Permissions:       getSubserverPermissions(),
				Registrar:         g,
				MacaroonValidator: g,
			}
			lisCfg := lnd.ListenerCfg{
				RPCListener: &lnd.ListenerWithSignal{
					Listener: &onDemandListener{
						addr: g.cfg.Lnd.RPCListeners[0],
					},
					Ready:                   readyChan,
					ExternalRPCSubserverCfg: extSubCfg,
					ExternalRestRegistrar:   g,
				},
				WalletUnlocker: &lnd.ListenerWithSignal{
					Listener: &onDemandListener{
						addr: g.cfg.Lnd.RPCListeners[0],
					},
					Ready: unlockChan,
				},
			}

			err := lnd.Main(
				g.cfg.Lnd, lisCfg, signal.ShutdownChannel(),
			)
			if e, ok := err.(*flags.Error); err != nil &&
				(!ok || e.Type != flags.ErrHelp) {

				log.Errorf("Error running main lnd: %v", err)
				g.lndErrChan <- err
				return
			}

			close(g.lndErrChan)
		}()
	} else {
		close(unlockChan)
		close(readyChan)

		_ = g.RegisterGrpcSubserver(g.rpcProxy.grpcServer)
	}

	// Wait for lnd to be started up so we know we have a TLS cert.
	select {
	// If lnd needs to be unlocked we get the signal that it's ready to do
	// so. We then go ahead and start the UI so we can unlock it there as
	// well.
	case <-unlockChan:

	// If lnd is running with --noseedbackup and doesn't need unlocking, we
	// get the ready signal immediately.
	case <-readyChan:

	case err := <-g.lndErrChan:
		return err

	case <-signal.ShutdownChannel():
		return errors.New("shutting down")
	}

	// We now know that starting lnd was successful. If we now run into an
	// error, we must shut down lnd correctly.
	defer func() {
		err := g.shutdown()
		if err != nil {
			log.Errorf("Error shutting down: %v", err)
		}
	}()

	// Now start the RPC proxy that will handle all incoming gRPC, grpc-web
	// and REST requests. We also start the main web server that dispatches
	// requests either to the static UI file server or the RPC proxy. This
	// makes it possible to unlock lnd through the UI.
	if err := g.rpcProxy.Start(); err != nil {
		return fmt.Errorf("error starting lnd gRPC proxy server: %v",
			err)
	}
	if err := g.startMainWebServer(); err != nil {
		return fmt.Errorf("error starting UI HTTP server: %v", err)
	}

	// Now that we have started the main UI web server, show some useful
	// information to the user so they can access the web UI easily.
	if err := g.showStartupInfo(); err != nil {
		return fmt.Errorf("error displaying startup info: %v", err)
	}

	// Wait for lnd to be unlocked, then start all clients.
	select {
	case <-readyChan:

	case err := <-g.lndErrChan:
		return err

	case <-signal.ShutdownChannel():
		return errors.New("shutting down")
	}

	err = g.startSubservers()
	if err != nil {
		log.Errorf("Could not start subservers: %v", err)
		return err
	}

	// Now block until we receive an error or the main shutdown signal.
	select {
	case err := <-g.loopServer.ErrChan:
		// Loop will shut itself down if an error happens. We don't need
		// to try to stop it again.
		g.loopStarted = false
		log.Errorf("Received critical error from loop, shutting down: "+
			"%v", err)

	case err := <-g.lndErrChan:
		if err != nil {
			log.Errorf("Received critical error from lnd, "+
				"shutting down: %v", err)
		}

	case <-signal.ShutdownChannel():
		log.Infof("Shutdown signal received")
	}

	return nil
}

// startSubservers creates an internal connection to lnd and then starts all
// embedded daemons as external subservers that hook into the same gRPC and REST
// servers that lnd started.
func (g *LightningTerminal) startSubservers() error {
	var basicClient lnrpc.LightningClient
	host, network, tlsPath, macPath := g.cfg.lndConnectParams()

	// The main RPC listener of lnd might need some time to start, it could
	// be that we run into a connection refused a few times. We use the
	// basic client connection to find out if the RPC server is started yet
	// because that doesn't do anything else than just connect. We'll check
	// if lnd is also ready to be used in the next step.
	err := wait.NoError(func() error {
		// Create an lnd client now that we have the full configuration.
		// We'll need a basic client and a full client because not all
		// subservers have the same requirements.
		var err error
		basicClient, err = lndclient.NewBasicClient(
			host, tlsPath, path.Dir(macPath), string(network),
			lndclient.MacFilename(path.Base(macPath)),
		)
		return err
	}, defaultStartupTimeout)
	if err != nil {
		return err
	}

	// Now we know that the connection itself is ready. But we also need to
	// wait for two things: The chain notifier to be ready and the lnd
	// wallet being fully synced to its chain backend. The chain notifier
	// will always be ready first so if we instruct the lndclient to wait
	// for the wallet sync, we should be fully ready to start all our
	// subservers. This will just block until lnd signals readiness. But we
	// still want to react to shutdown requests, so we need to listen for
	// those.
	ctxc, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Make sure the context is canceled if the user requests shutdown.
	go func() {
		select {
		// Client requests shutdown, cancel the wait.
		case <-signal.ShutdownChannel():
			cancel()

		// The check was completed and the above defer canceled the
		// context. We can just exit the goroutine, nothing more to do.
		case <-ctxc.Done():
		}
	}()
	g.lndClient, err = lndclient.NewLndServices(
		&lndclient.LndServicesConfig{
			LndAddress:            host,
			Network:               network,
			TLSPath:               tlsPath,
			CustomMacaroonPath:    macPath,
			BlockUntilChainSynced: true,
			BlockUntilUnlocked:    true,
			CallerCtx:             ctxc,
		},
	)
	if err != nil {
		return err
	}

	// Both connection types are ready now, let's start our subservers if
	// they should be started locally as an integrated service.
	if !g.cfg.faradayRemote {
		err = g.faradayServer.StartAsSubserver(g.lndClient.LndServices)
		if err != nil {
			return err
		}
		g.faradayStarted = true
	}

	if !g.cfg.loopRemote {
		err = g.loopServer.StartAsSubserver(g.lndClient)
		if err != nil {
			return err
		}
		g.loopStarted = true
	}

	if !g.cfg.poolRemote {
		err = g.poolServer.StartAsSubserver(basicClient, g.lndClient)
		if err != nil {
			return err
		}
		g.poolStarted = true
	}

	return nil
}

// RegisterGrpcSubserver is a callback on the lnd.SubserverConfig struct that is
// called once lnd has initialized its main gRPC server instance. It gives the
// daemons (or external subservers) the possibility to register themselves to
// the same server instance.
func (g *LightningTerminal) RegisterGrpcSubserver(server *grpc.Server) error {
	// In remote mode the "director" of the RPC proxy will act as a catch-
	// all for any gRPC request that isn't known because we didn't register
	// any server for it. The director will then forward the request to the
	// remote service.
	if !g.cfg.faradayRemote {
		frdrpc.RegisterFaradayServerServer(server, g.faradayServer)
	}

	if !g.cfg.loopRemote {
		looprpc.RegisterSwapClientServer(server, g.loopServer)
	}

	if !g.cfg.poolRemote {
		poolrpc.RegisterTraderServer(server, g.poolServer)
	}

	return nil
}

// RegisterRestSubserver is a callback on the lnd.SubserverConfig struct that is
// called once lnd has initialized its main REST server instance. It gives the
// daemons (or external subservers) the possibility to register themselves to
// the same server instance.
func (g *LightningTerminal) RegisterRestSubserver(ctx context.Context,
	mux *restProxy.ServeMux, endpoint string,
	dialOpts []grpc.DialOption) error {

	err := frdrpc.RegisterFaradayServerHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
	if err != nil {
		return err
	}

	err = looprpc.RegisterSwapClientHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
	if err != nil {
		return err
	}

	return poolrpc.RegisterTraderHandlerFromEndpoint(
		ctx, mux, endpoint, dialOpts,
	)
}

// ValidateMacaroon extracts the macaroon from the context's gRPC metadata,
// checks its signature, makes sure all specified permissions for the called
// method are contained within and finally ensures all caveat conditions are
// met. A non-nil error is returned if any of the checks fail.
func (g *LightningTerminal) ValidateMacaroon(ctx context.Context,
	requiredPermissions []bakery.Op, fullMethod string) error {

	// Validate all macaroons for services that are running in the local
	// process. Calls that we proxy to a remote host don't need to be
	// checked as they'll have their own interceptor.
	switch {
	case isFaradayURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.faradayRemote {
			return nil
		}

		if !g.faradayStarted {
			return fmt.Errorf("faraday is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		return g.faradayServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)

	case isLoopURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.loopRemote {
			return nil
		}

		if !g.loopStarted {
			return fmt.Errorf("loop is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		return g.loopServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)

	case isPoolURI(fullMethod):
		// In remote mode we just pass through the request, the remote
		// daemon will check the macaroon.
		if g.cfg.poolRemote {
			return nil
		}

		if !g.poolStarted {
			return fmt.Errorf("pool is not yet ready for " +
				"requests, lnd possibly still starting or " +
				"syncing")
		}

		return g.poolServer.ValidateMacaroon(
			ctx, requiredPermissions, fullMethod,
		)
	}

	// Because lnd will spin up its own gRPC server with macaroon
	// interceptors if it is running in this process, it will check its
	// macaroons there. If lnd is running remotely, that process will check
	// the macaroons. So we don't need to worry about anything other than
	// the subservers that are running in the local process.
	return nil
}

// shutdown stops all subservers that were started and attached to lnd.
func (g *LightningTerminal) shutdown() error {
	var returnErr error

	if g.faradayStarted {
		if err := g.faradayServer.Stop(); err != nil {
			log.Errorf("Error stopping faraday: %v", err)
			returnErr = err
		}
	}

	if g.loopStarted {
		g.loopServer.Stop()
		if err := <-g.loopServer.ErrChan; err != nil {
			log.Errorf("Error stopping loop: %v", err)
			returnErr = err
		}
	}

	if g.poolStarted {
		if err := g.poolServer.Stop(); err != nil {
			log.Errorf("Error stopping pool: %v", err)
			returnErr = err
		}
	}

	if g.lndClient != nil {
		g.lndClient.Close()
	}

	if g.rpcProxy != nil {
		if err := g.rpcProxy.Stop(); err != nil {
			log.Errorf("Error stopping lnd proxy: %v", err)
			returnErr = err
		}
	}

	if g.httpServer != nil {
		if err := g.httpServer.Close(); err != nil {
			log.Errorf("Error stopping UI server: %v", err)
			returnErr = err
		}
	}

	// In case the error wasn't thrown by lnd, make sure we stop it too.
	signal.RequestShutdown()

	g.wg.Wait()

	// The lnd error channel is only used if we are actually running lnd in
	// the same process.
	if g.cfg.LndMode == ModeIntegrated {
		err := <-g.lndErrChan
		if err != nil {
			log.Errorf("Error stopping lnd: %v", err)
			returnErr = err
		}
	}

	return returnErr
}

// startMainWebServer creates the main web HTTP server that delegates requests
// between the embedded HTTP server and the RPC proxy. An incoming request will
// go through the following chain of components:
//
//    Request on port 8443
//        |
//        v
//    +---+----------------------+ other  +----------------+
//    | Main web HTTP server     +------->+ Embedded HTTP  |
//    +---+----------------------+        +----------------+
//        |
//        v any RPC or REST call
//    +---+----------------------+
//    | grpc-web proxy           |
//    +---+----------------------+
//        |
//        v native gRPC call with basic auth
//    +---+----------------------+
//    | interceptors             |
//    +---+----------------------+
//        |
//        v native gRPC call with macaroon
//    +---+----------------------+
//    | gRPC server              |
//    +---+----------------------+
//        |
//        v unknown authenticated call, gRPC server is just a wrapper
//    +---+----------------------+
//    | director                 |
//    +---+----------------------+
//        |
//        v authenticated call
//    +---+----------------------+ call to lnd or integrated daemon
//    | lnd (remote or local)    +---------------+
//    | faraday remote           |               |
//    | loop remote              |    +----------v----------+
//    | pool remote              |    | lnd local subserver |
//    +--------------------------+    |  - faraday          |
//                                    |  - loop             |
//                                    |  - pool             |
//                                    +---------------------+
//
func (g *LightningTerminal) startMainWebServer() error {
	// Initialize the in-memory file server from the content compiled by
	// the go:embed directive. Since everything's relative to the root dir,
	// we need to create an FS of the sub directory app/build.
	buildDir, err := fs.Sub(appBuildFS, appFilesDir)
	if err != nil {
		return err
	}
	staticFileServer := http.FileServer(&ClientRouteWrapper{
		assets: http.FS(buildDir),
	})

	// Both gRPC (web) and static file requests will come into through the
	// main UI HTTP server. We use this simple switching handler to send the
	// requests to the correct implementation.
	httpHandler := func(resp http.ResponseWriter, req *http.Request) {
		// If this is some kind of gRPC, gRPC Web or REST call that
		// should go to lnd or one of the daemons, pass it to the proxy
		// that handles all those calls.
		if g.rpcProxy.isHandling(resp, req) {
			return
		}

		// If we got here, it's a static file the browser wants, or
		// something we don't know in which case the static file server
		// will answer with a 404.
		log.Infof("Handling static file request: %s", req.URL.Path)

		// Add 1-year cache header for static files. React uses content-
		// based hashes in file names, so when any file is updated, the
		// url will change causing the browser cached version to be
		// invalidated.
		var re = regexp.MustCompile(`^/(static|fonts|icons)/.*`)
		if re.MatchString(req.URL.Path) {
			resp.Header().Set("Cache-Control", "max-age=31536000")
		}

		// Transfer static files using gzip to save up to 70% of
		// bandwidth.
		gzipHandler := makeGzipHandler(staticFileServer.ServeHTTP)
		gzipHandler(resp, req)
	}

	// Create and start our HTTPS server now that will handle both gRPC web
	// and static file requests.
	g.httpServer = &http.Server{
		// To make sure that long-running calls and indefinitely opened
		// streaming connections aren't terminated by the internal
		// proxy, we need to disable all timeouts except the one for
		// reading the HTTP headers. That timeout shouldn't be removed
		// as we would otherwise be prone to the slowloris attack where
		// an attacker takes too long to send the headers and uses up
		// connections that way. Once the headers are read, we either
		// know it's a static resource and can deliver that very cheaply
		// or check the authentication for other calls.
		WriteTimeout:      0,
		IdleTimeout:       0,
		ReadTimeout:       0,
		ReadHeaderTimeout: defaultServerTimeout,
		Handler:           http.HandlerFunc(httpHandler),
	}
	httpListener, err := net.Listen("tcp", g.cfg.HTTPSListen)
	if err != nil {
		return fmt.Errorf("unable to listen on %v: %v",
			g.cfg.HTTPSListen, err)
	}
	tlsConfig, err := buildTLSConfigForHttp2(g.cfg)
	if err != nil {
		return fmt.Errorf("unable to create TLS config: %v", err)
	}
	tlsListener := tls.NewListener(httpListener, tlsConfig)

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()

		log.Infof("Listening for http_tls on: %v", tlsListener.Addr())
		err := g.httpServer.Serve(tlsListener)
		if err != nil && err != http.ErrServerClosed {
			log.Errorf("http_tls server error: %v", err)
		}
	}()

	// We only enable an additional HTTP only listener if the user
	// explicitly sets a value.
	if g.cfg.HTTPListen != "" {
		insecureListener, err := net.Listen("tcp", g.cfg.HTTPListen)
		if err != nil {
			return fmt.Errorf("unable to listen on %v: %v",
				g.cfg.HTTPListen, err)
		}

		g.wg.Add(1)
		go func() {
			defer g.wg.Done()

			log.Infof("Listening for http on: %v",
				insecureListener.Addr())
			err := g.httpServer.Serve(insecureListener)
			if err != nil && err != http.ErrServerClosed {
				log.Errorf("http server error: %v", err)
			}
		}()
	}

	return nil
}

// showStartupInfo shows useful information to the user to easily access the
// web UI that was just started.
func (g *LightningTerminal) showStartupInfo() error {
	info := struct {
		mode    string
		status  string
		alias   string
		version string
		webURI  string
	}{
		mode:    g.cfg.LndMode,
		status:  "locked",
		alias:   g.cfg.Lnd.Alias,
		version: build.Version(),
		webURI: fmt.Sprintf("https://%s", strings.ReplaceAll(
			strings.ReplaceAll(
				g.cfg.HTTPSListen, "0.0.0.0", "localhost",
			), "[::]", "localhost",
		)),
	}

	// In remote mode we try to query the info.
	if g.cfg.LndMode == ModeRemote {
		// We try to query GetInfo on the remote node to find out the
		// alias. But the wallet might be locked.
		host, network, tlsPath, macPath := g.cfg.lndConnectParams()
		basicClient, err := lndclient.NewBasicClient(
			host, tlsPath, path.Dir(macPath), string(network),
			lndclient.MacFilename(path.Base(macPath)),
		)
		if err != nil {
			return fmt.Errorf("error querying remote node: %v", err)
		}

		ctx := context.Background()
		res, err := basicClient.GetInfo(ctx, &lnrpc.GetInfoRequest{})
		if err != nil {
			s, ok := status.FromError(err)
			if !ok || s.Code() != codes.Unimplemented {
				// Some other error that we didn't expect at
				// this moment.
				return fmt.Errorf("error querying remote "+
					"node : %v", err)
			}

			// Node is locked.
			info.status = "locked"
			info.alias = "???? (node is locked)"
		} else {
			info.status = "online"
			info.alias = res.Alias
			info.version = res.Version
		}
	}

	// In integrated mode, we can derive the state from our configuration.
	if g.cfg.LndMode == ModeIntegrated {
		// If the integrated node is running with no seed backup, the
		// wallet cannot be locked and the node is online right away.
		if g.cfg.Lnd.NoSeedBackup {
			info.status = "online"
		}
	}

	// If there's an additional HTTP listener, list it as well.
	if g.cfg.HTTPListen != "" {
		host := strings.ReplaceAll(
			strings.ReplaceAll(
				g.cfg.HTTPListen, "0.0.0.0", "localhost",
			), "[::]", "localhost",
		)
		info.webURI = fmt.Sprintf("%s, http://%s", info.webURI, host)
	}

	str := "" +
		"----------------------------------------------------------\n" +
		" Lightning Terminal (LiT) by Lightning Labs               \n" +
		"                                                          \n" +
		" Operating mode      %s                                   \n" +
		" Node status         %s                                   \n" +
		" Alias               %s                                   \n" +
		" Version             %s                                   \n" +
		" Web interface       %s                                   \n" +
		"----------------------------------------------------------\n"
	fmt.Printf(str, info.mode, info.status, info.alias, info.version,
		info.webURI)

	return nil
}

// ClientRouteWrapper is a wrapper around a FileSystem which properly handles
// URL routes that are defined in the client app but unknown to the backend
// http server
type ClientRouteWrapper struct {
	assets http.FileSystem
}

// Open intercepts requests to open files. If the file does not exist and there
// is no file extension, then assume this is a client side route and return the
// contents of index.html
func (i *ClientRouteWrapper) Open(name string) (http.File, error) {
	ret, err := i.assets.Open(name)
	if !os.IsNotExist(err) || filepath.Ext(name) != "" {
		return ret, err
	}

	return i.assets.Open("/index.html")
}
