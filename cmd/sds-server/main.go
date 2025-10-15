package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sider/internal/server"
)

func main() {
	var httpAddr string
	var raftID, raftBind, raftDir string
	var bootstrap bool
	flag.StringVar(&httpAddr, "http", ":8500", "HTTP 监听地址，如 :8500 或 127.0.0.1:8500")
	flag.StringVar(&raftID, "raft-id", "node1", "Raft 节点 ID（集群内唯一）")
	flag.StringVar(&raftBind, "raft-bind", "127.0.0.1:8501", "Raft 监听地址（host:port）")
	flag.StringVar(&raftDir, "raft-dir", "data/raft", "Raft 数据目录")
	flag.BoolVar(&bootstrap, "raft-bootstrap", true, "是否作为引导节点（首次启动单节点集群）")
	flag.Parse()

	ctx, cancel := signalContext()
	defer cancel()

	srv := &server.Server{HTTPAddr: httpAddr, RaftID: raftID, RaftBind: raftBind, RaftDir: raftDir, Bootstrap: bootstrap}
	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server exited with error: %v", err)
	}
}

// signalContext: 在收到 SIGINT/SIGTERM 时取消的 context。
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 2)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-c
		// small grace period to allow shutdown logs to flush
		go func() {
			time.Sleep(2 * time.Second)
			os.Exit(0)
		}()
		cancel()
	}()
	return ctx, cancel
}
