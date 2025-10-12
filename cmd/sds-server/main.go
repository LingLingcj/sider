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
    flag.StringVar(&httpAddr, "http", ":8500", "HTTP 监听地址，如 :8500 或 127.0.0.1:8500")
    flag.Parse()

    ctx, cancel := signalContext()
    defer cancel()

    srv := &server.Server{HTTPAddr: httpAddr}
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
