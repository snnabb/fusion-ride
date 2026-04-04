package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fusionride/fusion-ride/internal/admin"
	"github.com/fusionride/fusion-ride/internal/aggregator"
	"github.com/fusionride/fusion-ride/internal/auth"
	"github.com/fusionride/fusion-ride/internal/config"
	"github.com/fusionride/fusion-ride/internal/db"
	"github.com/fusionride/fusion-ride/internal/idmap"
	"github.com/fusionride/fusion-ride/internal/logger"
	"github.com/fusionride/fusion-ride/internal/proxy"
	"github.com/fusionride/fusion-ride/internal/traffic"
	"github.com/fusionride/fusion-ride/internal/upstream"
	"github.com/fusionride/fusion-ride/web"
)

// Server 是 FusionRide 的 HTTP 服务器。
type Server struct {
	cfg       *config.Config
	cfgPath   string
	db        *db.DB
	log       *logger.Logger
	httpSrv   *http.Server
	upMgr     *upstream.Manager
	meter     *traffic.Meter
	adminAPI  *admin.API
}

// New 创建服务器实例。
func New(cfg *config.Config, database *db.DB, log *logger.Logger) *Server {
	return &Server{
		cfg:     cfg,
		cfgPath: "config/config.yaml",
		db:      database,
		log:     log,
	}
}

// Start 启动 HTTP 服务器。
func (s *Server) Start(ctx context.Context) error {
	// ── 初始化子系统 ──

	// 管理员认证
	adminAuth := auth.NewAdminAuth(s.db, "")

	// 上游管理
	s.upMgr = upstream.NewManager(s.db, s.log, s.cfg.Playback.Mode)

	// ID 映射
	ids := idmap.NewStore(s.db)

	// 流量计量
	s.meter = traffic.NewMeter(s.db)
	s.meter.StartFlush(60 * time.Second)

	// 聚合鳌引擎
	agg := aggregator.New(
		s.upMgr, ids, s.log,
		time.Duration(s.cfg.Timeouts.Aggregate)*time.Millisecond,
		s.cfg.Bitrate.CodecPriority,
	)

	// 代理处理器
	proxyHandler := proxy.NewHandler(s.cfg, s.upMgr, agg, ids, s.log, s.meter)

	// 管理面板 API
	s.adminAPI = admin.NewAPI(s.cfg, s.cfgPath, adminAuth, s.upMgr, ids, s.log, s.meter)

	// 启动 SSE 流量广播
	s.adminAPI.StartTrafficBroadcast(s.meter, 2*time.Second)

	// 启动健康检查
	s.upMgr.StartHealthChecks(
		time.Duration(s.cfg.Timeouts.HealthInterval)*time.Millisecond,
		time.Duration(s.cfg.Timeouts.HealthCheck)*time.Millisecond,
	)

	// 尝试认证所有上游
	for _, u := range s.upMgr.All() {
		if u.Enabled && u.Session == nil {
			go s.upMgr.Reconnect(u.ID)
		}
	}

	// ── 路由 ──
	mux := http.NewServeMux()

	// 管理面板 API
	adminHandler := s.adminAPI.Handler()
	mux.Handle("/admin/api/", adminHandler)

	// 管理面板静态文件
	staticFS := web.StaticFS()
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	mux.Handle("/admin/", http.StripPrefix("/admin/", http.FileServer(http.FS(staticFS))))

	// Emby API 代理（所有其他请求）
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 跳过 /admin 路径
		if strings.HasPrefix(r.URL.Path, "/admin") {
			return
		}
		proxyHandler.ServeHTTP(w, r)
	})

	// ── 中间件链 ──
	handler := s.recoveryMiddleware(
		s.corsMiddleware(
			s.loggingMiddleware(mux),
		),
	)

	s.httpSrv = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.cfg.Server.Port),
		Handler:      handler,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0, // 流媒体不设写超时
		IdleTimeout:  120 * time.Second,
	}

	s.log.Info("FusionRide 启动在端口 %d", s.cfg.Server.Port)
	s.log.Info("  管理面板: http://localhost:%d/admin/", s.cfg.Server.Port)
	s.log.Info("  Emby 连接: http://localhost:%d", s.cfg.Server.Port)

	if adminAuth.NeedsSetup() {
		s.log.Info("  ⚡ 首次运行，请访问管理面板设置管理员密码")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return nil
	}
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.log.Info("正在关闭服务...")

	if s.upMgr != nil {
		s.upMgr.Stop()
	}
	if s.meter != nil {
		s.meter.Stop()
	}
	if s.httpSrv != nil {
		s.httpSrv.Shutdown(ctx)
	}

	s.log.Info("服务已关闭")
}

// ── 中间件 ──

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: 200}

		next.ServeHTTP(rw, r)

		// 不记录静态资源和 SSE
		if !strings.HasSuffix(r.URL.Path, ".js") &&
			!strings.HasSuffix(r.URL.Path, ".css") &&
			!strings.Contains(r.URL.Path, "/stream") &&
			r.URL.Path != "/admin/api/traffic/stream" {

			s.log.Debug("%s %s → %d (%s)",
				r.Method, r.URL.Path, rw.statusCode, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Emby 客户端需要的 CORS 头
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Emby-Token, X-Emby-Authorization, X-Emby-Client, X-Emby-Device-Name, X-Emby-Device-Id, X-Emby-Client-Version")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				s.log.Error("PANIC: %v [%s %s]", err, r.Method, r.URL.Path)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter 包装 http.ResponseWriter 以捕获状态码。
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	hijacked   bool
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (c interface{}, brw interface{}, err error) {
	// Not implementing full Hijack for simplicity
	return nil, nil, fmt.Errorf("hijack not supported through wrapper")
}
