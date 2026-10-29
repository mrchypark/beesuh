package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mrchypark/beesuh"
	"github.com/mrchypark/beesuh/compos"
	"github.com/mrchypark/beesuh/conductor"
	"github.com/mrchypark/beesuh/httpapi"
	"github.com/mrchypark/beesuh/model"
	"github.com/mrchypark/beesuh/opencode"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func closeWithError(err *error, close func() error) {
	*err = errors.Join(*err, close())
}

func run() (err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	var (
		backend             = flag.String("backend", "native", "runtime backend: native or opencode")
		openCodeURL         = flag.String("opencode-url", "http://127.0.0.1:4096", "OpenCode server URL")
		directory           = flag.String("directory", cwd, "absolute OpenCode working directory")
		dataDir             = flag.String("data-dir", ".beesuh", "Rhiza data directory")
		storageInit         = flag.Bool("storage-init", false, "initialize and validate retained local Rhiza storage without starting services; main uses the same -data-dir")
		backendID           = flag.String("backend-id", "local", "OpenCode backend identity")
		session             = flag.String("session", "", "required logical session key")
		composInput         = flag.Bool("compos", false, "read Compos agent outbox data from stdin")
		message             = flag.String("message", "", "direct user message")
		workspaceID         = flag.String("workspace", "", "workspace ID for a direct message")
		requestID           = flag.String("request-id", "", "request ID for a direct message")
		maxConcurrent       = flag.Int("max-concurrent", 4, "maximum concurrent sessions")
		maxWaiting          = flag.Int("max-waiting", 32, "maximum queued requests")
		retainedRuns        = flag.Int("retained-runs", 10000, "maximum retained runs per workspace")
		totalRuns           = flag.Int("retained-runs-total", 100000, "maximum retained runs in the data directory")
		retainedSessions    = flag.Int("retained-sessions", 1000, "maximum retained sessions per workspace")
		totalSessions       = flag.Int("retained-sessions-total", 10000, "maximum retained sessions in the data directory")
		pendingCreates      = flag.Int("pending-session-creates", 1000, "maximum pending session creations per workspace")
		totalCreates        = flag.Int("pending-session-creates-total", 10000, "maximum pending session creations in the data directory")
		retainedConnections = flag.Int("retained-connections", 1000, "maximum retained connections per workspace")
		totalConnections    = flag.Int("retained-connections-total", 10000, "maximum retained connections in the data directory")
		timeout             = flag.Duration("timeout", 5*time.Minute, "maximum request duration")
		recoverOnly         = flag.Bool("recover", false, "abort and reconcile uncertain owned sessions")
		conductorTools      = flag.Bool("conductor-tools", false, "enable configured Conductor tools")
		providersFile       = flag.String("providers", "", "native provider registry file")
		providerID          = flag.String("provider", "", "native provider ID from -providers")
		modelName           = flag.String("model", os.Getenv("BEESUH_MODEL"), "native model ID, or provider/model with -backend opencode")
		modelURL            = flag.String("model-url", os.Getenv("BEESUH_MODEL_URL"), "native OpenAI-compatible model URL")
		system              = flag.String("system", "", "native system prompt")
		skillsDir           = flag.String("skills-dir", "", "native skills directory")
		memory              = flag.Bool("memory", false, "enable native durable memory")
		maxTurns            = flag.Int("max-turns", 12, "native maximum model/tool turns")
		maxContext          = flag.Int("max-context-bytes", 65536, "native maximum saved context bytes")
		maxOutput           = flag.Int("max-output-tokens", 2048, "native maximum completion tokens")
		tokenBudget         = flag.String("token-budget-policy", string(beesuh.TokenBudgetStrict), "native token budget policy: strict, advisory, or unlimited")
		interactive         = flag.Bool("interactive", false, "read direct chat messages, one line at a time")
		listen              = flag.String("listen", "", "serve authenticated chat HTTP; requires -service-config, -backend native and -providers")
		serviceFile         = flag.String("service-config", "", "OAuth2/OIDC service configuration file")
		writeTimeout        = flag.Duration("write-timeout", 0, "maximum HTTP response duration; 0 derives it from -timeout plus the handler write budget")
	)
	flag.Parse()
	if *storageInit {
		if flag.NArg() != 0 {
			return errors.New("unexpected positional arguments")
		}
		var incompatible string
		flag.Visit(func(f *flag.Flag) {
			if f.Name != "storage-init" && f.Name != "data-dir" && incompatible == "" {
				incompatible = f.Name
			}
		})
		if incompatible != "" {
			return fmt.Errorf("-storage-init cannot be combined with -%s", incompatible)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return beesuh.InitStorage(ctx, *dataDir)
	}
	if *listen != "" && (strings.TrimSpace(*serviceFile) == "" || *backend != "native" || strings.TrimSpace(*providersFile) == "") {
		return errors.New("-listen requires -service-config, -backend native and -providers")
	}
	if *listen != "" && *conductorTools {
		return errors.New("-listen forbids operator Conductor configuration")
	}
	if *serviceFile != "" && (*listen == "" || *backend != "native" || *providersFile == "") {
		return errors.New("-service-config requires -listen, -backend native and -providers")
	}
	if !filepath.IsAbs(*directory) {
		return errors.New("-directory must be absolute")
	}
	options := beesuh.Options{
		DataDir:       *dataDir,
		BackendID:     *backendID,
		MaxConcurrent: *maxConcurrent,
		MaxWaiting:    *maxWaiting,
		Timeout:       *timeout,
		Capacity: beesuh.CapacityLimits{RunsPerWorkspace: *retainedRuns, RunsGlobal: *totalRuns,
			SessionsPerWorkspace: *retainedSessions, SessionsGlobal: *totalSessions,
			PendingCreatesPerWorkspace: *pendingCreates, PendingCreatesGlobal: *totalCreates,
			ConnectionsPerWorkspace: *retainedConnections, ConnectionsGlobal: *totalConnections},
	}
	if *backend == "opencode" {
		username := os.Getenv("OPENCODE_SERVER_USERNAME")
		if username == "" {
			username = "opencode"
		}
		options.OpenCode = opencode.Config{BaseURL: *openCodeURL, Directory: *directory, Username: username, Password: os.Getenv("OPENCODE_SERVER_PASSWORD")}
	}
	var requestProvider string
	switch *backend {
	case "native":
		var native *beesuh.NativeOptions
		if strings.TrimSpace(*providersFile) != "" {
			if explicitFlag("model") || explicitFlag("model-url") {
				return errors.New("-providers cannot be combined with -model or -model-url")
			}
			loadProviders := model.LoadProviders
			if *serviceFile != "" {
				loadProviders = model.LoadServiceProviders
			}
			providers, defaultProvider, err := loadProviders(*providersFile)
			if err != nil {
				return err
			}
			native = &beesuh.NativeOptions{Providers: providers, DefaultProvider: defaultProvider, System: *system, SkillsDir: *skillsDir, Memory: *memory, MaxTurns: *maxTurns, MaxContextBytes: *maxContext, MaxOutputTokens: *maxOutput, TokenBudgetPolicy: beesuh.TokenBudgetPolicy(*tokenBudget)}
			requestProvider = *providerID
		} else {
			if strings.TrimSpace(*providerID) != "" {
				return errors.New("-provider requires -providers")
			}
			if strings.TrimSpace(*modelURL) == "" || strings.TrimSpace(*modelName) == "" {
				return errors.New("-model-url (or BEESUH_MODEL_URL) and -model (or BEESUH_MODEL) are required with -backend native")
			}
			native = &beesuh.NativeOptions{Provider: model.Config{BaseURL: *modelURL, APIKey: os.Getenv("BEESUH_MODEL_API_KEY"), Model: *modelName}, System: *system, SkillsDir: *skillsDir, Memory: *memory, MaxTurns: *maxTurns, MaxContextBytes: *maxContext, MaxOutputTokens: *maxOutput, TokenBudgetPolicy: beesuh.TokenBudgetPolicy(*tokenBudget)}
		}
		if *listen != "" && requestProvider != "" {
			native.DefaultProvider = requestProvider
			requestProvider = ""
		}
		if *conductorTools {
			var bindings map[string]conductor.Binding
			if err := json.Unmarshal([]byte(os.Getenv("CONDUCTOR_EVENT_BINDINGS")), &bindings); err != nil {
				return errors.New("CONDUCTOR_EVENT_BINDINGS must be a JSON object of handler bindings")
			}
			native.Conductor = &conductor.Config{Bindings: bindings, BaseURL: os.Getenv("CONDUCTOR_URL"), Token: os.Getenv("CONDUCTOR_TOKEN"), AllowedEvents: split(os.Getenv("CONDUCTOR_ALLOWED_EVENTS"))}
		}
		options.Native = native
	case "opencode":
		if strings.TrimSpace(*providersFile) != "" || strings.TrimSpace(*providerID) != "" {
			return errors.New("-providers and -provider require -backend native")
		}
		permissions := []opencode.PermissionRule{{Permission: "*", Pattern: "*", Action: "deny"}}
		if *conductorTools {
			permissions = append(permissions,
				opencode.PermissionRule{Permission: "conductor_get_run", Pattern: "*", Action: "allow"},
			)
		}
		options.Permissions = permissions
		if *modelName != "" {
			provider, name, ok := strings.Cut(*modelName, "/")
			if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(name) == "" {
				return errors.New("-model must be provider/model with -backend opencode")
			}
			options.Model = &opencode.Model{ProviderID: provider, ModelID: name}
		}
	default:
		return errors.New("-backend must be native or opencode")
	}
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *listen != "" && (*recoverOnly || *composInput || *message != "" || *interactive) {
		return errors.New("-listen cannot be combined with one-shot modes")
	}
	if *recoverOnly && (*composInput || *message != "" || *interactive) {
		return errors.New("-recover cannot be combined with an input")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var service *serviceConfig
	if *serviceFile != "" {
		service, err = loadService(*serviceFile)
		if err != nil {
			return err
		}
		if err = service.configure(&options); err != nil {
			return err
		}
	}
	runtime, err := beesuh.Open(ctx, options)
	if err != nil {
		return err
	}
	defer closeWithError(&err, runtime.Close)
	if *listen != "" {
		handler, err := httpapi.NewAuthenticated(runtime, service.authenticate)
		if err != nil {
			return err
		}
		handler = service.discoveryHandler(runtime, handler)
		listener, err := net.Listen("tcp", *listen)
		if err != nil {
			return err
		}
		// The write budget must cover the full request plus the handler's bounded
		// response write. A client that stops reading is cut off; it can refetch the
		// durable result with a new request.
		write := *writeTimeout
		if write <= 0 {
			write = *timeout + 2*httpapi.WriteBudget
		}
		server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: write, IdleTimeout: 60 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
		finished := make(chan error, 1)
		go func() { finished <- server.Serve(listener) }()
		fmt.Fprintln(os.Stderr, "beesuh listening on", listener.Addr())
		select {
		case err := <-finished:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdown); err != nil {
				_ = server.Close()
				return err
			}
			return nil
		}
	}
	if *recoverOnly {
		return runtime.Recover(ctx)
	}
	if strings.TrimSpace(*session) == "" {
		return errors.New("-session is required")
	}
	if *interactive {
		if *composInput || *message != "" || strings.TrimSpace(*workspaceID) == "" {
			return errors.New("-interactive requires -workspace and -session, and cannot be combined with -compos or -message")
		}
		return runInteractive(ctx, runtime, *workspaceID, *session, requestProvider)
	}
	if *composInput == (*message != "") {
		return errors.New("select exactly one of -compos or nonempty -message")
	}
	var request beesuh.Request
	if *composInput {
		request, err = compos.Decode(os.Stdin, *session)
		request.Provider = requestProvider
	} else {
		if strings.TrimSpace(*workspaceID) == "" || strings.TrimSpace(*requestID) == "" {
			return errors.New("-workspace and -request-id are required with -message")
		}
		request = beesuh.Request{
			WorkspaceID: *workspaceID,
			SessionKey:  *session,
			RequestID:   *requestID,
			Provider:    requestProvider,
			Prompt:      beesuh.Prompt{Text: *message},
		}
	}
	if err != nil {
		return err
	}
	result, runErr := runtime.Run(ctx, request)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		return err
	}
	return runErr
}

func runInteractive(ctx context.Context, runtime *beesuh.Runtime, workspace, session, provider string) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	lines := make(chan string)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		var line string
		select {
		case <-ctx.Done():
			return nil
		case value, ok := <-lines:
			if !ok {
				return scanner.Err()
			}
			line = value
		}
		if line == "/quit" {
			return nil
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		out, err := runtime.Run(ctx, beesuh.Request{WorkspaceID: workspace, SessionKey: session, RequestID: "req_" + rand.Text(), Provider: provider, Prompt: beesuh.Prompt{Text: line}})
		if encodeErr := json.NewEncoder(os.Stdout).Encode(out); encodeErr != nil {
			return encodeErr
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
}

func explicitFlag(name string) bool {
	found := false
	flag.Visit(func(current *flag.Flag) { found = found || current.Name == name })
	return found
}

func split(value string) []string {
	var out []string
	for _, value := range strings.Split(value, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}
