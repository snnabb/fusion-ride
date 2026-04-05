package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/snnabb/fusion-ride/internal/admin"
	"github.com/snnabb/fusion-ride/internal/aggregator"
	"github.com/snnabb/fusion-ride/internal/auth"
	"github.com/snnabb/fusion-ride/internal/config"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/idmap"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/proxy"
	"github.com/snnabb/fusion-ride/internal/traffic"
	"github.com/snnabb/fusion-ride/internal/upstream"
	"github.com/snnabb/fusion-ride/web"
)

type Server struct {
	cfg      *config.Config
	cfgPath  string
	db       *db.DB
	log      *logger.Logger
	httpSrv  *http.Server
	upMgr    *upstream.Manager
	meter    *traffic.Meter
	adminAPI *admin.API
}

func New(cfg *config.Config, cfgPath string, database *db.DB, log *logger.Logger) *Server {
	return &Server{
		cfg:     cfg,
		cfgPath: cfgPath,
		db:      database,
		log:     log,
	}
}

func (s *Server) Start(ctx context.Context) error {
	adminAuth := auth.NewAdminAuth(s.db, "")
	if adminAuth.NeedsSetup() && strings.TrimSpace(s.cfg.Admin.Password) != "" {
		if err := adminAuth.Setup(s.cfg.Admin.Username, s.cfg.Admin.Password); err != nil {
			return fmt.Errorf("初始化管理员凭据失败: %w", err)
		}
	}
	s.upMgr = upstream.NewManager(s.db, s.log, s.cfg.Playback.Mode)
	ids := idmap.NewStore(s.db)

	s.meter = traffic.NewMeter(s.db)
	s.meter.StartFlush(60 * time.Second)

	agg := aggregator.New(
		s.upMgr,
		ids,
		s.log,
		time.Duration(s.cfg.Timeouts.Aggregate)*time.Millisecond,
		s.cfg.Bitrate.CodecPriority,
	)
	proxyHandler := proxy.NewHandler(s.cfg, s.db, s.upMgr, agg, ids, s.log, s.meter, adminAuth)
	proxyHandler.StartSessionCleanup(ctx)

	s.adminAPI = admin.NewAPI(s.cfg, s.cfgPath, adminAuth, s.upMgr, ids, s.log, s.meter)
	s.adminAPI.StartTrafficBroadcast(s.meter, 2*time.Second)

	s.upMgr.StartHealthChecks(
		time.Duration(s.cfg.Timeouts.HealthInterval)*time.Millisecond,
		time.Duration(s.cfg.Timeouts.HealthCheck)*time.Millisecond,
	)

	for _, upstreamInstance := range s.upMgr.All() {
		if upstreamInstance.Enabled && upstreamInstance.Session == nil {
			go s.upMgr.Reconnect(upstreamInstance.ID)
		}
	}

	s.httpSrv = &http.Server{
		Addr:         fmt.Sprintf(":%d", s.cfg.Server.Port),
		Handler:      s.buildHandler(proxyHandler),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	s.log.Info("FusionRide 启动于端口 %d", s.cfg.Server.Port)
	s.log.Info("管理面板地址：http://localhost:%d/admin/", s.cfg.Server.Port)
	s.log.Info("代理入口地址：http://localhost:%d", s.cfg.Server.Port)
	if adminAuth.NeedsSetup() {
		s.log.Info("首次运行，请访问管理面板完成管理员初始化")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

func (s *Server) Shutdown(ctx context.Context) {
	s.log.Info("正在关闭服务")

	if s.upMgr != nil {
		s.upMgr.Stop()
	}
	if s.meter != nil {
		s.meter.Stop()
	}
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil {
			s.log.Warn("关闭 HTTP 服务时发生错误: %v", err)
		}
	}

	s.log.Info("服务已关闭")
}

func (s *Server) buildHandler(proxyHandler *proxy.Handler) http.Handler {
	mux := http.NewServeMux()
	adminHandler := s.adminAPI.Handler()
	staticFS := web.StaticFS()

	mux.Handle("/admin/api/", adminHandler)
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	mux.Handle("/admin/", http.StripPrefix("/admin/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/", proxyHandler.ServeHTTP)

	return s.recoveryMiddleware(
		s.corsMiddleware(
			s.loggingMiddleware(mux),
		),
	)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "仅支持 GET", http.StatusMethodNotAllowed)
		return
	}

	total := 0
	online := 0
	if s.upMgr != nil {
		total = len(s.upMgr.All())
		online = len(s.upMgr.Online())
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": "ok",
		"upstreams": map[string]int{
			"online": online,
			"total":  total,
		},
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(rw, r)

		if !strings.HasSuffix(r.URL.Path, ".js") &&
			!strings.HasSuffix(r.URL.Path, ".css") &&
			!strings.Contains(r.URL.Path, "/stream") &&
			r.URL.Path != "/admin/api/traffic/stream" {
			s.log.Debug("%s %s -> %d (%s)", r.Method, r.URL.Path, rw.statusCode, time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Emby-Token, X-Emby-Authorization, X-Emby-Client, X-Emby-Device-Name, X-Emby-Device-Id, X-Emby-Client-Version")

		if r.Method == http.MethodOptions {
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
				s.log.Error("请求处理发生 panic: %v [%s %s]", err, r.Method, r.URL.Path)
				http.Error(w, "内部服务错误", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("当前响应不支持连接劫持")
	}
	return hijacker.Hijack()
}
