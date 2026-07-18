package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	edgeservice "github.com/QuantumNous/new-api/service/edge"
	_ "github.com/QuantumNous/new-api/setting/performance_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

type Config struct {
	Mode        common.RuntimeMode
	ThemeAssets router.ThemeAssets
}

func Run(config Config) (runErr error) {
	if err := common.SetRuntimeMode(config.Mode); err != nil {
		return err
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	startTime := time.Now()
	databaseInitialized, err := initResources(config.Mode)
	if err != nil {
		return fmt.Errorf("failed to initialize resources: %w", err)
	}

	if config.Mode == common.RuntimeModeEdge {
		common.SysLog("New API Edge " + common.Version + " started")
	} else {
		common.SysLog("New API " + common.Version + " started")
	}
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}
	if common.DebugEnabled {
		common.SysLog("running in debug mode")
	}

	if databaseInitialized {
		defer func() {
			if err := model.CloseDB(); err != nil && runErr == nil {
				runErr = fmt.Errorf("failed to close database: %w", err)
			}
		}()
	}
	if config.Mode == common.RuntimeModeEdge {
		if err := edgeservice.InitializeEdgeAccountingReadiness(context.Background(), model.DB); err != nil {
			return fmt.Errorf("failed to initialize edge accounting readiness: %w", err)
		}
	}

	backgroundContext, cancelBackground := context.WithCancel(context.Background())
	var backgroundWorkers sync.WaitGroup
	var stopBackgroundOnce sync.Once
	stopBackground := func() {
		stopBackgroundOnce.Do(func() {
			cancelBackground()
			backgroundWorkers.Wait()
		})
	}
	defer stopBackground()
	if config.Mode == common.RuntimeModeEdge {
		service.SetEdgeBillingSessionFactory(edgeservice.BillingSessionFactory)
		defer service.SetEdgeBillingSessionFactory(nil)
		edgeservice.SetEdgeRequestAdmission(true)
		defer edgeservice.SetEdgeRequestAdmission(false)
		backgroundWorkers.Add(1)
		go func() {
			defer backgroundWorkers.Done()
			if err := edgeservice.RunEdgeControlLoops(backgroundContext); err != nil && !errors.Is(err, context.Canceled) {
				common.SysError("edge control loops stopped: " + err.Error())
			}
		}()
		backgroundWorkers.Add(1)
		go func() {
			defer backgroundWorkers.Done()
			edgeservice.RunEdgeAccountingMaintenance(backgroundContext)
		}()
		backgroundWorkers.Add(1)
		go func() {
			defer backgroundWorkers.Done()
			common.RunSystemMonitor(backgroundContext)
		}()
	}

	if config.Mode == common.RuntimeModeMaster {
		if common.RedisEnabled {
			// for compatibility with old versions
			common.MemoryCacheEnabled = true
		}
		if common.MemoryCacheEnabled {
			common.SysLog("memory cache enabled")
			common.SysLog(fmt.Sprintf("sync frequency: %d seconds", common.SyncFrequency))

			// Add panic recovery and retry for InitChannelCache
			func() {
				defer func() {
					if r := recover(); r != nil {
						common.SysLog(fmt.Sprintf("InitChannelCache panic: %v, retrying once", r))
						_, _, fixErr := model.FixAbility()
						if fixErr != nil {
							common.FatalLog(fmt.Sprintf("InitChannelCache failed: %s", fixErr.Error()))
						}
					}
				}()
				model.InitChannelCache()
			}()

			go model.SyncChannelCache(common.SyncFrequency)
		}

		// Warm pricing after channel cache initialization so Advanced Custom
		// endpoint inference can read cached route settings on first request.
		model.GetPricing()

		go model.SyncOptions(common.SyncFrequency)

		if strings.EqualFold(strings.TrimSpace(os.Getenv("EDGE_DISTRIBUTED_ENABLED")), "true") {
			if _, err := edgeservice.CompileAndPublishEdgeSnapshot(); err != nil {
				// Edge-unrepresentable configuration must not take the master
				// API down; edges keep serving the last published snapshot and
				// the periodic compile loop keeps reporting the error.
				common.SysError("initial edge snapshot compilation failed, master starts degraded: " + err.Error())
			}
			backgroundWorkers.Add(1)
			go func() {
				defer backgroundWorkers.Done()
				edgeservice.RunMasterConsumeLogOutbox(backgroundContext)
			}()
			compileInterval := common.GetEnvOrDefault("EDGE_SNAPSHOT_COMPILE_INTERVAL_SECONDS", 900)
			if compileInterval < 60 {
				compileInterval = 60
			}
			backgroundWorkers.Add(1)
			go func() {
				defer backgroundWorkers.Done()
				ticker := time.NewTicker(time.Duration(compileInterval) * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-backgroundContext.Done():
						return
					case <-ticker.C:
						if _, err := edgeservice.CompileAndPublishEdgeSnapshot(); err != nil {
							common.SysError("periodic edge snapshot compilation failed: " + err.Error())
						}
					}
				}
			}()
		}
	}

	if config.Mode == common.RuntimeModeMaster {
		// 周期性重载授权策略，保证多节点/多 master 部署下权限变更能传播到每个实例
		go authz.StartPolicySync(common.SyncFrequency)

		// 数据看板
		go model.UpdateQuotaData()

		if os.Getenv("CHANNEL_UPDATE_FREQUENCY") != "" {
			frequency, err := strconv.Atoi(os.Getenv("CHANNEL_UPDATE_FREQUENCY"))
			if err != nil {
				return fmt.Errorf("failed to parse CHANNEL_UPDATE_FREQUENCY: %w", err)
			}
			go controller.AutomaticallyUpdateChannels(frequency)
		}

		// Codex credential auto-refresh check every 10 minutes, refresh when expires within 1 day
		service.StartCodexCredentialAutoRefreshTask()

		// Subscription quota reset task (daily/weekly/monthly/custom)
		service.StartSubscriptionQuotaResetTask()

		// Report this process as a system instance so the System Info page can show
		// all currently alive nodes in multi-instance deployments.
		service.StartSystemInstanceReporter()

		// Wire task polling adaptor factory (breaks service -> relay import cycle).
		// Must run before the system task runner starts: the async_task_poll handler
		// calls service.RunTaskPollingOnce, which needs this factory set.
		service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
			a := relay.GetTaskAdaptor(platform)
			if a == nil {
				return nil
			}
			return a
		}

		// Register the periodic channel test, upstream model update, and async task
		// polling jobs, then start the master-only runner.
		controller.RegisterScheduledSystemTasks()
		service.StartSystemTaskRunner()

		if os.Getenv("BATCH_UPDATE_ENABLED") == "true" {
			common.BatchUpdateEnabled = true
			common.SysLog("batch update enabled with interval " + strconv.Itoa(common.BatchUpdateInterval) + "s")
			model.InitBatchUpdater()
		}
	}

	if config.Mode == common.RuntimeModeMaster && os.Getenv("ENABLE_PPROF") == "true" {
		gopool.Go(func() {
			log.Println(http.ListenAndServe("0.0.0.0:8005", nil))
		})
		go common.Monitor()
		common.SysLog("pprof enabled")
	}

	if err := common.StartPyroScope(); err != nil {
		common.SysError(fmt.Sprintf("start pyroscope error : %v", err))
	}

	server := newHTTPServer(config)
	port := os.Getenv("PORT")
	if port == "" {
		port = strconv.Itoa(*common.Port)
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: server,
	}
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- srv.Serve(listener)
	}()

	time.Sleep(100 * time.Millisecond)
	common.LogStartupSuccess(startTime, port)

	var serveErr error
	select {
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("HTTP server stopped unexpectedly: %w", err)
		}
	case sig := <-quit:
		common.SysLog(fmt.Sprintf("received signal: %v, shutting down...", sig))
	}

	// SSE streams may run for minutes; give them time to finish before forced exit.
	if config.Mode == common.RuntimeModeEdge {
		edgeservice.SetEdgeRequestAdmission(false)
	}
	shutdownTimeout := time.Duration(common.GetEnvOrDefault("SHUTDOWN_TIMEOUT_SECONDS", 120)) * time.Second
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), shutdownTimeout)
	if err := srv.Shutdown(shutdownContext); err != nil {
		common.SysError(fmt.Sprintf("server forced to shutdown: %v", err))
		if closeErr := srv.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
			common.SysError(fmt.Sprintf("failed to close HTTP server connections: %v", closeErr))
		}
	}
	cancelShutdown()
	if config.Mode == common.RuntimeModeEdge {
		// Closing active connections cancels their request contexts, but handlers
		// may still need a short unwind to settle durable usage. Keep waiting even
		// after the graceful deadline: closing SQLite underneath those handlers can
		// lose accounting state, so database safety takes priority over fast exit.
		if err := edgeservice.WaitForEdgeRequests(context.Background()); err != nil {
			return errors.Join(serveErr, fmt.Errorf("wait for edge requests: %w", err))
		}

		drainClient, drainClientAvailable := edgeservice.ActiveEdgeControlClient()
		drainMaxEvents := 100
		if control, exists := edgeservice.ActiveEdgeControlConfig(); exists && control.SettlementMaxEvents > 0 {
			drainMaxEvents = control.SettlementMaxEvents
		}
		stopBackground()
		if !drainClientAvailable {
			common.SysError("edge control drain incomplete; durable state retained: edge control client is unavailable")
		} else {
			drainTimeout := time.Duration(common.GetEnvOrDefault("EDGE_DRAIN_TIMEOUT_SECONDS", 30)) * time.Second
			drainContext, cancelDrain := context.WithTimeout(context.Background(), drainTimeout)
			if err := edgeservice.DrainEdgeControlWithClient(drainContext, drainClient, drainMaxEvents); err != nil {
				common.SysError("edge control drain incomplete; durable state retained: " + err.Error())
			}
			cancelDrain()
		}
	}
	if config.Mode == common.RuntimeModeMaster && common.DataExportEnabled {
		// 内存中的看板数据保存入库，避免重启丢失未落库数据 (issue #5679)
		model.SaveQuotaDataCache()
	}
	common.SysLog("server exited")
	return serveErr
}

func newHTTPServer(config Config) *gin.Engine {
	server := gin.New()
	server.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		common.SysLog(fmt.Sprintf("panic detected: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Panic detected, error: %v. Please submit a issue here: https://github.com/Calcium-Ion/new-api", err),
				"type":    "new_api_panic",
			},
		})
	}))
	server.Use(middleware.RequestId())
	server.Use(middleware.Version())
	server.Use(middleware.I18n())
	middleware.SetUpLogger(server)

	if config.Mode == common.RuntimeModeEdge {
		router.SetEdgeRouter(server)
		return server
	}

	store := cookie.NewStore([]byte(common.SessionSecret))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   2592000, // 30 days
		HttpOnly: true,
		Secure:   common.SessionCookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
	server.Use(sessions.Sessions("session", store))

	assets := injectAnalytics(config.ThemeAssets)
	router.SetRouter(server, assets)
	return server
}

func injectAnalytics(assets router.ThemeAssets) router.ThemeAssets {
	umamiInjectBuilder := &strings.Builder{}
	if os.Getenv("UMAMI_WEBSITE_ID") != "" {
		umamiSiteID := os.Getenv("UMAMI_WEBSITE_ID")
		umamiScriptURL := os.Getenv("UMAMI_SCRIPT_URL")
		if umamiScriptURL == "" {
			umamiScriptURL = "https://analytics.umami.is/script.js"
		}
		umamiInjectBuilder.WriteString("<script defer src=\"")
		umamiInjectBuilder.WriteString(umamiScriptURL)
		umamiInjectBuilder.WriteString("\" data-website-id=\"")
		umamiInjectBuilder.WriteString(umamiSiteID)
		umamiInjectBuilder.WriteString("\"></script>")
	}
	umamiInjectBuilder.WriteString("<!--Umami QuantumNous-->\n")
	umamiInject := []byte(umamiInjectBuilder.String())
	umamiPlaceholder := []byte("<!--umami-->\n")
	assets.DefaultIndexPage = bytes.ReplaceAll(assets.DefaultIndexPage, umamiPlaceholder, umamiInject)
	assets.ClassicIndexPage = bytes.ReplaceAll(assets.ClassicIndexPage, umamiPlaceholder, umamiInject)

	googleInjectBuilder := &strings.Builder{}
	if os.Getenv("GOOGLE_ANALYTICS_ID") != "" {
		gaID := os.Getenv("GOOGLE_ANALYTICS_ID")
		googleInjectBuilder.WriteString("<script async src=\"https://www.googletagmanager.com/gtag/js?id=")
		googleInjectBuilder.WriteString(gaID)
		googleInjectBuilder.WriteString("\"></script>")
		googleInjectBuilder.WriteString("<script>")
		googleInjectBuilder.WriteString("window.dataLayer = window.dataLayer || [];")
		googleInjectBuilder.WriteString("function gtag(){dataLayer.push(arguments);}")
		googleInjectBuilder.WriteString("gtag('js', new Date());")
		googleInjectBuilder.WriteString("gtag('config', '")
		googleInjectBuilder.WriteString(gaID)
		googleInjectBuilder.WriteString("');")
		googleInjectBuilder.WriteString("</script>")
	}
	googleInjectBuilder.WriteString("<!--Google Analytics QuantumNous-->\n")
	googleInject := []byte(googleInjectBuilder.String())
	googlePlaceholder := []byte("<!--Google Analytics-->\n")
	assets.DefaultIndexPage = bytes.ReplaceAll(assets.DefaultIndexPage, googlePlaceholder, googleInject)
	assets.ClassicIndexPage = bytes.ReplaceAll(assets.ClassicIndexPage, googlePlaceholder, googleInject)
	return assets
}

func initResources(mode common.RuntimeMode) (bool, error) {
	if err := godotenv.Load(".env"); err != nil && common.DebugEnabled {
		common.SysLog("No .env file found, using default environment variables. If needed, please create a .env file and set the relevant variables.")
	}

	common.InitEnv()
	if mode == common.RuntimeModeEdge && !common.IsMasterNode {
		return false, errors.New("edge runtime cannot use legacy NODE_TYPE=slave shared-database mode")
	}

	logger.SetupLogger()
	ratio_setting.InitRatioSettings()
	service.InitHttpClient()
	service.InitTokenEncoders()

	if mode == common.RuntimeModeEdge {
		if err := model.InitEdgeDB(); err != nil {
			return false, err
		}
		if err := initI18n(); err != nil {
			common.SysError("failed to initialize i18n: " + err.Error())
		}
		return true, nil
	}

	if err := model.InitDB(); err != nil {
		return false, err
	}
	if err := authz.Init(model.DB); err != nil {
		return false, fmt.Errorf("failed to initialize authorization: %w", err)
	}
	model.CheckSetup()

	model.InitOptionMap()
	common.CleanupOldCacheFiles()

	if err := model.InitLogDB(); err != nil {
		return false, err
	}
	if err := common.InitRedisClient(); err != nil {
		return false, err
	}

	perfmetrics.Init()
	common.StartSystemMonitor()
	if err := initI18n(); err != nil {
		common.SysError("failed to initialize i18n: " + err.Error())
	}
	i18n.SetUserLangLoader(model.GetUserLanguage)

	if err := oauth.LoadCustomProviders(); err != nil {
		common.SysError("failed to load custom OAuth providers: " + err.Error())
	}

	return true, nil
}

func initI18n() error {
	if err := i18n.Init(); err != nil {
		return err
	}
	common.SysLog("i18n initialized with languages: " + strings.Join(i18n.SupportedLanguages(), ", "))
	return nil
}
