package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/pprof"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	connectip "github.com/iselt/connect-ip-go"
	common "github.com/iselt/masque-vpn/common"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/yosida95/uritemplate/v3"
)

var clientConfig common.ClientConfig

func main() {
	if os.Getenv("PERF_PROFILE") != "" {
		f, _ := os.OpenFile("cpu.pprof", os.O_CREATE|os.O_RDWR, 0666)
		defer f.Close()
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	// --- 配置加载 ---
	configFile := flag.String("c", "config.client.toml", "Config file path")
	flag.Parse()
	if _, err := toml.DecodeFile(*configFile, &clientConfig); err != nil {
		log.Fatalf("Error loading config file %s: %v", *configFile, err)
	}

	// --- 基础验证 ---
	if clientConfig.ServerAddr == "" || clientConfig.ServerName == "" {
		log.Fatal("Missing required configuration values (server_addr, server_name) in config.client.toml")
	}

	log.Printf("Starting VPN Client...")
	log.Printf("Server Address: %s", clientConfig.ServerAddr)
	log.Printf("Server Name: %s", clientConfig.ServerName)
	if clientConfig.InsecureSkipVerify {
		log.Println("WARNING: Skipping TLS server verification!")
	}

	// --- 创建用于优雅关闭的 Context ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var tunDev *common.TUNDevice
	var ipConn *connectip.Conn

	// --- 建立连接并配置 TUN 设备 ---
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		tunDev, ipConn, err = establishAndConfigure(ctx)
		if err != nil {
			log.Printf("Failed to establish connection: %v", err)
			stop() // Signal main goroutine to exit if setup fails
			return
		}
		log.Println("Connection established and TUN device configured.")
		// --- 启动代理 Goroutine ---
		errChan := make(chan error, 2)
		var proxyWg sync.WaitGroup

		proxyWg.Add(2)
		go func() {
			defer proxyWg.Done()
			common.ProxyFromTunToVPN(tunDev, ipConn, errChan)
		}()
		go func() {
			defer proxyWg.Done()
			common.ProxyFromVPNToTun(tunDev, ipConn, errChan)
		}()

		// --- 等待错误或关闭信号 ---
		select {
		case err := <-errChan:
			log.Printf("Proxying error: %v", err)
		case <-ctx.Done():
			log.Println("Shutdown signal received, stopping proxy...")
		}

		// --- 清理 ---
		log.Println("Closing connection and TUN device...")
		if ipConn != nil {
			ipConn.Close()
		}
		if tunDev != nil {
			tunDev.Close() // Closing the TUN device should unblock reads/writes
		}

		// Wait for proxy goroutines to finish
		proxyWg.Wait()
		log.Println("Proxy goroutines finished.")
	}()

	// Wait for the main goroutine (establishAndConfigure + proxying) to finish or be signaled
	wg.Wait()
	log.Println("VPN Client exited.")
}

// establishAndConfigure 函数，用于连接服务器，设置 TUN 设备和路由
func establishAndConfigure(ctx context.Context) (*common.TUNDevice, *connectip.Conn, error) {
	// --- TLS 配置 ---
	tlsConfig := &tls.Config{
		ServerName:         clientConfig.ServerName,
		InsecureSkipVerify: clientConfig.InsecureSkipVerify,
		NextProtos:         []string{http3.NextProtoH3}, // Required for http3
	}
	// 优先从 PEM 字符串加载 CA
	if clientConfig.CAPEM != "" {
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM([]byte(clientConfig.CAPEM)) {
			return nil, nil, fmt.Errorf("failed to append CA cert from config ca_pem")
		}
		tlsConfig.RootCAs = caCertPool
		tlsConfig.InsecureSkipVerify = false
		log.Printf("Using custom CA from config ca_pem")
	} else if clientConfig.CAFile != "" {
		caCert, err := os.ReadFile(clientConfig.CAFile)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read CA file %s: %w", clientConfig.CAFile, err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, nil, fmt.Errorf("failed to append CA cert from %s", clientConfig.CAFile)
		}
		tlsConfig.RootCAs = caCertPool
		tlsConfig.InsecureSkipVerify = false
		log.Printf("Using custom CA file: %s", clientConfig.CAFile)
	}
	// 优先从 PEM 字符串加载证书和密钥
	if clientConfig.CertPEM != "" && clientConfig.KeyPEM != "" {
		cert, err := tls.X509KeyPair([]byte(clientConfig.CertPEM), []byte(clientConfig.KeyPEM))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load client certificate/key from config PEM: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		log.Printf("Loaded client certificate/key from config PEM")
	} else if clientConfig.TLSCert != "" && clientConfig.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(clientConfig.TLSCert, clientConfig.TLSKey)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to load client certificate/key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		log.Printf("Loaded client certificate: %s", clientConfig.TLSCert)
	} else {
		return nil, nil, fmt.Errorf("tls_cert and tls_key or cert_pem and key_pem must be set in config for mutual TLS authentication")
	}
	if clientConfig.KeyLogFile != "" {
		keyLogWriter, err := os.OpenFile(clientConfig.KeyLogFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			log.Printf("Warning: failed to create key log file %s: %v", clientConfig.KeyLogFile, err)
		} else {
			tlsConfig.KeyLogWriter = keyLogWriter
			defer keyLogWriter.Close() // Close when function returns error or finishes setup
			log.Printf("Logging TLS keys to: %s", clientConfig.KeyLogFile)
		}
	}

	// --- QUIC 连接 ---
	quicConf := &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  60 * time.Second,
		KeepAlivePeriod: 30 * time.Second,
	}

	log.Printf("Dialing QUIC connection to %s...", clientConfig.ServerAddr)
	// 我们需要一个 UDP socket 来进行拨号
	udpConn, err := net.ListenUDP("udp", nil) // Let OS choose source IP/port
	if err != nil {
		return nil, nil, fmt.Errorf("failed to listen on UDP: %w", err)
	}
	// defer udpConn.Close() // Close underlying UDP conn when QUIC conn closes or setup fails

	serverUdpAddr, err := net.ResolveUDPAddr("udp", clientConfig.ServerAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve server address %s: %w", clientConfig.ServerAddr, err)
	}

	// 使用带有超时的 context 进行拨号
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second) // 15 sec dial timeout
	defer dialCancel()

	quicConn, err := quic.Dial(dialCtx, udpConn, serverUdpAddr, tlsConfig, quicConf)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial QUIC connection to %s: %w", clientConfig.ServerAddr, err)
	}
	log.Printf("QUIC connection established to %s", quicConn.RemoteAddr())
	// Note: quicConn.Close() will be called implicitly when ipConn.Close() is called later.

	// --- HTTP/3 和 CONNECT-IP ---
	h3RoundTripper := &http3.Transport{
		EnableDatagrams: true,
		QUICConfig:      quicConf, // Can reuse config, or nil
	}
	// 创建一个 H3 客户端连接包装器
	h3ClientConn := h3RoundTripper.NewClientConn(quicConn)

	// 使用配置的服务器名称和端口作为模板
	// serverHost, serverPortStr, _ := net.SplitHostPort(clientConfig.ServerAddr)
	_, serverPortStr, _ := net.SplitHostPort(clientConfig.ServerAddr)
	serverPort, _ := strconv.Atoi(serverPortStr)
	template := uritemplate.MustNew(fmt.Sprintf("https://%s:%d/vpn", clientConfig.ServerName, serverPort)) // Use configured server name

	log.Printf("Dialing CONNECT-IP via HTTP/3...")
	connectCtx, connectCancel := context.WithTimeout(ctx, 10*time.Second) // 10 sec connect-ip timeout
	defer connectCancel()

	ipConn, resp, err := connectip.Dial(connectCtx, h3ClientConn, template)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial connect-ip: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 尝试读取 body 获取更多信息
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, nil, fmt.Errorf("connect-ip dial failed, server returned status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	// resp.Body.Close()
	log.Printf("CONNECT-IP session established.")

	// --- 从服务器获取分配的 IP 和路由 ---
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 5*time.Second)
	defer fetchCancel()

	// 获取从服务器分配的网络前缀
	localPrefixes, err := ipConn.LocalPrefixes(fetchCtx)
	if err != nil {
		ipConn.Close()
		return nil, nil, fmt.Errorf("failed to get assigned network prefix: %w", err)
	}

	if len(localPrefixes) == 0 {
		ipConn.Close()
		return nil, nil, errors.New("server did not assign any network prefix")
	}
	log.Printf("Received network prefix: %v", localPrefixes)

	// 新逻辑：直接使用服务器分配的唯一 IP 前缀
	assignedPrefix := localPrefixes[0]
	log.Printf("Using assigned TUN IP: %s", assignedPrefix)

	dev, err := common.CreateTunDevice(clientConfig.TunName, assignedPrefix, clientConfig.MTU)
	if err != nil {
		ipConn.Close()
		return nil, nil, fmt.Errorf("failed to create and configure TUN device: %w", err)
	}
	log.Printf("TUN device %s configured with IP %s", dev.Name(), assignedPrefix)

	routes, err := ipConn.Routes(fetchCtx)
	if err != nil {
		ipConn.Close()
		return nil, nil, fmt.Errorf("failed to get advertised routes: %w", err)
	}

	log.Printf("Received advertised routes: %v", routes)
	// 添加服务器通告的路由
	// var networkPrefix netip.Prefix
	// if len(localPrefixes) > 0 {
	// 	// 取第一个前缀作为网络前缀
	// 	networkPrefix = localPrefixes[0]
	// }
	// 添加自己的路由
	// dev.AddRoute(networkPrefix)

	addedRoutes := 0
	for _, route := range routes {
		log.Printf("Processing route: Start=%s, End=%s, Proto=%d", route.StartIP, route.EndIP, route.IPProtocol)

		for _, prefix := range route.Prefixes() {
			// 跳过与我们自己的网络前缀匹配的路由
			// if networkPrefix.IsValid() && networkPrefix.Contains(prefix.Addr()) {
			// 	log.Printf("Skipping route %s as it's part of our network prefix %s", prefix, networkPrefix)
			// 	continue
			// }

			// 直接使用TUN设备对象添加路由
			if err := dev.AddRoute(prefix); err != nil {
				log.Printf("Warning: failed to add route for %s: %v", prefix, err)
			} else {
				log.Printf("Added route: %s via %s", prefix, dev.Name())
				addedRoutes++
			}
		}
	}
	log.Printf("Added %d routes from server's advertisement", addedRoutes)

	// --- 添加持续监听地址和路由更新的协程 ---
	continusUpdate := true // 是否持续更新地址和路由
	if continusUpdate {
		go monitorAddressAndRouteUpdates(ctx, ipConn, dev)
	}

	// 返回配置的 TUN 设备和活动的 connect-ip 连接
	return dev, ipConn, nil
}

// 监控地址和路由更新的协程
func monitorAddressAndRouteUpdates(ctx context.Context, conn *connectip.Conn, tunDev *common.TUNDevice) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 检查地址更新
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			localPrefixes, err := conn.LocalPrefixes(checkCtx)
			cancel()

			if err == nil && len(localPrefixes) > 0 {
				log.Printf("Checking for IP address updates, current prefixes: %v", localPrefixes)
				// 这里可以添加处理地址变更的逻辑
				// 目前仅记录，实际应用中可能需要更新TUN设备地址
				log.Printf("Current TUN device(%s) IP: %s", tunDev.Name(), localPrefixes[0])
				// 例如：如果需要更新 TUN 设备的 IP 地址，可以在这里调用 common.CreateTunDevice 函数
			}

			// 检查路由更新
			checkCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
			routes, err := conn.Routes(checkCtx)
			cancel()

			if err == nil && len(routes) > 0 {
				log.Printf("Checking for route updates, current routes: %d routes", len(routes))
				// 这里可以添加处理路由变更的逻辑
				// 目前仅记录，实际应用中可能需要更新路由表
				for _, route := range routes {
					log.Printf("Route: Start=%s, End=%s, Proto=%d", route.StartIP, route.EndIP, route.IPProtocol)
					for _, prefix := range route.Prefixes() {
						log.Printf("Route Prefix: %s", prefix)
					}
				}
			}
		}
	}
}
