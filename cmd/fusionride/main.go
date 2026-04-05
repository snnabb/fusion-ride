package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/snnabb/fusion-ride/internal/config"
	"github.com/snnabb/fusion-ride/internal/db"
	"github.com/snnabb/fusion-ride/internal/logger"
	"github.com/snnabb/fusion-ride/internal/server"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	cfgPath := flag.String("config", "config/config.yaml", "配置文件路径")
	dataDir := flag.String("data-dir", "data/", "数据目录")
	showVersion := flag.Bool("version", false, "显示版本信息")
	flag.String("db", "", "(已废弃)")
	flag.Int("port", 0, "(已废弃)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("FusionRide %s (built %s)\n", Version, BuildTime)
		os.Exit(0)
	}

	if _, err := os.Stat(*cfgPath); os.IsNotExist(err) {
		defaultCfg := config.Default()
		if saveErr := defaultCfg.Save(*cfgPath); saveErr != nil {
			fmt.Fprintf(os.Stderr, "生成默认配置失败: %v\n", saveErr)
			os.Exit(1)
		}
		fmt.Printf("已生成默认配置: %s\n", *cfgPath)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	log := logger.New(filepath.Join(*dataDir, "fusionride.log"))
	log.Info("FusionRide %s 启动中", Version)

	database, err := db.Open(filepath.Join(*dataDir, "fusionride.db"))
	if err != nil {
		log.Error("打开数据库失败: %v", err)
		os.Exit(1)
	}
	defer database.Close()

	srv := server.New(cfg, *cfgPath, database, log)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := srv.Start(ctx); err != nil {
			log.Error("服务启动失败: %v", err)
			cancel()
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	srv.Shutdown(shutdownCtx)
}
