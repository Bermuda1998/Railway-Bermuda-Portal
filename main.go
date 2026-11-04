package main

import (
	"context"
	"errors"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPort               = "8080"
	compatHttpPort            = "80"
	fallbackSelfMemMB         = 128
	fallbackWorkerMemMB       = 550
	defaultGOMAXPROCS         = 2
	gatewayMemPercent         = 14
	workerMemPercent          = 54
	httpDrainTimeout          = 8 * time.Second
	edgeKeepAlivePeriod       = 15 * time.Second
	drainPropagationWindow    = 450 * time.Millisecond
	startupOrchestrationDelay = 3 * time.Second
	dnsPropagationSettleDelay = 4 * time.Second
	tcpUserTimeoutOpt         = 18
)

func cgroupMemoryLimit() (uint64, bool) {
	candidates := []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" || s == "max" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil || n == 0 || n >= 1<<50 {
			continue
		}
		return n, true
	}
	return 0, false
}

func deriveMemoryBudget() (gwMB, workerMB int) {
	gwMB, workerMB = fallbackSelfMemMB, fallbackWorkerMemMB
	limit, limited := cgroupMemoryLimit()
	if limited {
		totalMB := int(limit >> 20)
		gwMB = totalMB * gatewayMemPercent / 100
		workerMB = totalMB * workerMemPercent / 100

		allowedMB := totalMB * 68 / 100
		if gwMB < 48 {
			gwMB = 48
		}
		if workerMB < 64 {
			workerMB = 64
		}
		if total := gwMB + workerMB; total > allowedMB && allowedMB >= 112 {
			gwMB = allowedMB * gatewayMemPercent / 68
			workerMB = allowedMB * workerMemPercent / 68
		}
	}

	gwMB = getEnvInt("BERMUDA_SELF_MEM_MB", gwMB)
	workerMB = getEnvInt("BERMUDA_WORKER_MEM_MB", getEnvInt("BERMUDA_XRAY_MEM_MB", workerMB))
	return gwMB, workerMB
}

func applyMemoryCeiling(selfMB int) {
	if selfMB < 48 {
		selfMB = 48
	}
	debug.SetMemoryLimit(int64(selfMB) << 20)
	debug.SetGCPercent(100)
}

func cgroupCPUQuota() (float64, bool) {
	if data, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		f := strings.Fields(string(data))
		if len(f) == 2 && f[0] != "max" {
			q, e1 := strconv.ParseFloat(f[0], 64)
			p, e2 := strconv.ParseFloat(f[1], 64)
			if e1 == nil && e2 == nil && q > 0 && p > 0 {
				return q / p, true
			}
		}
	}
	qb, e1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	pb, e2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if e1 == nil && e2 == nil {
		q, e3 := strconv.ParseFloat(strings.TrimSpace(string(qb)), 64)
		p, e4 := strconv.ParseFloat(strings.TrimSpace(string(pb)), 64)
		if e3 == nil && e4 == nil && q > 0 && p > 0 {
			return q / p, true
		}
	}
	return 0, false
}

func applyGOMAXPROCS() int {
	n := getEnvInt("BERMUDA_GOMAXPROCS", 0)
	if n == 0 {
		if q, ok := cgroupCPUQuota(); ok {
			n = int(math.Ceil(q))
		}
	}
	if n < 2 {
		n = defaultGOMAXPROCS
	}
	if cpus := runtime.NumCPU(); n > cpus && cpus > 0 {
		n = cpus
	}
	runtime.GOMAXPROCS(n)
	return n
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func cleanHost(h string) string {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "${{") || strings.HasPrefix(h, "$") {
		return ""
	}
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	if slashIdx := strings.IndexByte(h, '/'); slashIdx != -1 {
		h = h[:slashIdx]
	}
	h = strings.TrimRight(h, "/.")
	if idx := strings.Index(h, ":"); idx != -1 && !strings.Contains(h, "]") {
		h = h[:idx]
	}
	return strings.ToLower(strings.TrimSpace(h))
}

func isRailwayCnameAlias(h string) bool {
	h = cleanHost(h)
	if h == "" {
		return false
	}
	if h == "railway.app" || h == "up.railway.app" || h == "railway.internal" {
		return true
	}
	return strings.HasSuffix(h, ".railway.internal")
}

func isRailwayManagedDomain(h string) bool {
	h = cleanHost(h)
	if h == "" {
		return false
	}
	return isRailwayCnameAlias(h) ||
		strings.Contains(h, "railway.app") ||
		strings.HasSuffix(h, "-service") ||
		strings.HasSuffix(h, ".cluster.local") ||
		strings.HasSuffix(h, ".svc") ||
		strings.HasSuffix(h, ".internal") ||
		strings.HasSuffix(h, ".local")
}

func isRailwayPublicDomain(h string) bool {
	h = cleanHost(h)
	if h == "" || isRailwayCnameAlias(h) {
		return false
	}
	return strings.Contains(h, "railway.app") &&
		!strings.HasSuffix(h, ".internal") &&
		!strings.HasSuffix(h, ".local") &&
		!strings.HasSuffix(h, ".cluster.local") &&
		!strings.HasSuffix(h, ".svc") &&
		!strings.HasSuffix(h, "-service")
}

func isFullRailwayServiceDomain(h string) bool {
	h = cleanHost(h)
	if !strings.HasSuffix(h, ".up.railway.app") {
		return false
	}
	sub := strings.TrimSuffix(h, ".up.railway.app")
	return sub != ""
}

func normalizeRegion(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(s, "europe") || strings.Contains(s, "amsterdam") || strings.Contains(s, "drams") || strings.Contains(s, "nl") || strings.Contains(s, "ams"):
		return "europe-west4"
	case strings.Contains(s, "de-fra") || strings.Contains(s, "frankfurt") || strings.Contains(s, "fra") || strings.Contains(s, "eu-central") || strings.Contains(s, "germany"):
		return "de-fra1"
	case strings.Contains(s, "fi-hel") || strings.Contains(s, "helsinki") || strings.Contains(s, "finland") || strings.Contains(s, "hel"):
		return "fi-hel1"
	case strings.Contains(s, "uk-lon") || strings.Contains(s, "london") || strings.Contains(s, "uk"):
		return "uk-lon1"
	case strings.Contains(s, "us-east") || strings.Contains(s, "virginia") || strings.Contains(s, "eqdc") || strings.Contains(s, "iad") || strings.Contains(s, "nyc"):
		return "us-east4"
	case strings.Contains(s, "us-west") || strings.Contains(s, "california") || strings.Contains(s, "oregon") || strings.Contains(s, "sjc"):
		return "us-west"
	case strings.Contains(s, "asia") || strings.Contains(s, "singapore") || strings.Contains(s, "eqsg") || strings.Contains(s, "sin") || strings.Contains(s, "ap-"):
		return "asia-southeast1"
	default:
		return "europe-west4"
	}
}

func resolveInfrastructureDomains() (railwayDomain, customDomain string) {
	staticURL := cleanHost(getEnv("RAILWAY_STATIC_URL", ""))
	publicDomain := cleanHost(getEnv("RAILWAY_PUBLIC_DOMAIN", getEnv("RAILWAY_DOMAIN", "")))
	explicitRailway := cleanHost(getEnv("RAILWAY_DEFAULT_DOMAIN", getEnv("BERMUDA_RAILWAY_DOMAIN", "")))
	customEnv := cleanHost(getEnv("CUSTOM_DOMAIN", getEnv("BERMUDA_CUSTOM_DOMAIN", "")))

	for _, candidate := range []string{explicitRailway, staticURL, publicDomain} {
		if isRailwayPublicDomain(candidate) {
			railwayDomain = candidate
			break
		}
	}

	for _, candidate := range []string{customEnv, publicDomain, staticURL, explicitRailway} {
		if candidate != "" && !isRailwayManagedDomain(candidate) {
			customDomain = candidate
			break
		}
	}

	return railwayDomain, customDomain
}

func orchestrateCloudInfrastructure(
	ctx context.Context,
	sup *Supervisor,
	gw *Gateway,
	rw *RailwayClient,
	cf *CloudflareClient,
	initialRW string,
	initialCF string,
	region string,
) {
	for i := 0; i < 120 && !sup.IsReady(); i++ {
		if !sleepContext(ctx, 250*time.Millisecond) {
			return
		}
	}

	if !sleepContext(ctx, startupOrchestrationDelay) {
		return
	}

	railwayDomain := cleanHost(initialRW)
	customDomain := cleanHost(initialCF)
	if curRW, curCF, _ := sup.GetInfrastructureDomains(); railwayDomain == "" || customDomain == "" {
		if railwayDomain == "" && isRailwayPublicDomain(curRW) {
			railwayDomain = curRW
		}
		if customDomain == "" && curCF != "" && !isRailwayManagedDomain(curCF) {
			customDomain = curCF
		}
	}

	topologyUpdated := false

	syncDomains := func(newRW, newCF string) {
		cleanRW := cleanHost(newRW)
		cleanCF := cleanHost(newCF)
		if isRailwayPublicDomain(cleanRW) {
			if railwayDomain == "" || (!isFullRailwayServiceDomain(railwayDomain) && isFullRailwayServiceDomain(cleanRW)) {
				if railwayDomain != cleanRW {
					railwayDomain = cleanRW
					topologyUpdated = true
				}
			}
		}
		if cleanCF != "" && !isRailwayManagedDomain(cleanCF) {
			if customDomain == "" || strings.HasSuffix(cleanCF, "."+customDomain) {
				if customDomain != cleanCF {
					customDomain = cleanCF
					topologyUpdated = true
				}
			}
		}
		sup.SetInfrastructureDomains(railwayDomain, customDomain, region)
		gw.SetInfrastructureDomains(railwayDomain, customDomain, region)
		if cf != nil {
			if railwayDomain != "" {
				cf.SetRailwayOrigin(railwayDomain)
			}
			if customDomain != "" {
				cf.SetPreferredDomain(customDomain)
			}
		}
		if rw != nil {
			if railwayDomain != "" {
				rw.SetServiceDomain(railwayDomain)
			}
			if customDomain != "" {
				rw.SetCustomDomain(customDomain)
			}
		}
	}

	if rw != nil && rw.IsConfigured() {
		if (sup.IsMasterKeyAutoGenerated() || sanitizeMasterKey(getEnv("BERMUDA_MASTER_KEY", getEnv("MASTER_KEY", ""))) == "") &&
			!strings.EqualFold(getEnv("BERMUDA_PERSIST_KEY", "true"), "false") {
			rw.PersistMasterKey(ctx, sup.MasterKey())
		}

		if srvDom, err := rw.EnsureServiceDomain(ctx); err == nil && srvDom != "" {
			syncDomains(srvDom, "")
		} else if err != nil {
			log.Printf("[Railway] Notice: Service domain lookup: %v", err)
		}

		if reg := rw.GetDiscoveredRegion(); reg != "" {
			region = normalizeRegion(reg)
			sup.SetInfrastructureDomains(railwayDomain, customDomain, region)
			gw.SetInfrastructureDomains(railwayDomain, customDomain, region)
		}
	}

	if cf != nil && cf.Token != "" {
		probeCtx, probeCancel := context.WithTimeout(ctx, 12*time.Second)
		if cfDom, err := cf.FetchZoneDomain(probeCtx); err == nil && cfDom != "" {
			syncDomains(cf.GetRailwayOrigin(), cfDom)
		} else if err != nil {
			log.Printf("[Cloudflare] Notice: Zone domain discovery: %v", err)
		}
		probeCancel()
	}

	if topologyUpdated && ctx.Err() == nil {
		log.Printf("[Runtime] Synchronized Topology: RailwayDomain=%q | CloudflareDomain=%q | Region=%q",
			railwayDomain, customDomain, region)
		sup.LogProvisioningLink("")
	}

	var customDomInfo *RailwayCustomDomainInfo
	var cnameRecordID string
	var cfDNSProvisioned bool

	if customDomain != "" && (rw != nil || railwayDomain != "") {
		if rw != nil && rw.IsConfigured() {
			info, err := rw.EnsureCustomDomain(ctx, customDomain)
			if err != nil {
				log.Printf("[Railway] Notice: Custom domain registration (%s): %v", customDomain, err)
			} else if info != nil {
				customDomInfo = info
				syncDomains(rw.GetServiceDomain(), info.Domain)
			}
		}
		if customDomInfo == nil && railwayDomain != "" {
			certState := "CERTIFICATE_STATUS_TYPE_ISSUING"
			isValid := probeCustomDomainTLS(ctx, customDomain, railwayDomain)
			if isValid {
				certState = "CERTIFICATE_STATUS_TYPE_VALID"
			}
			customDomInfo = &RailwayCustomDomainInfo{
				Domain:            customDomain,
				CNAMETarget:       railwayDomain,
				CertificateStatus: certState,
				RecordsVerified:   isValid,
			}
		}
		if customDomInfo != nil && cf != nil && cf.Token != "" {
			recID, dnsErr := cf.ProvisionRailwayDNS(ctx, customDomInfo)
			if dnsErr != nil {
				log.Printf("[Cloudflare] Warning: Automated Railway DNS provisioning: %v", dnsErr)
			} else {
				cnameRecordID = recID
				cfDNSProvisioned = true
				_ = sleepContext(ctx, dnsPropagationSettleDelay)
			}
		}
	}

	if cf != nil && cf.Token != "" && ctx.Err() == nil {
		cf.Tune(ctx)
		syncDomains(cf.GetRailwayOrigin(), cf.CachedZoneDomain())
	}

	autoOrange := !strings.EqualFold(getEnv("BERMUDA_CF_AUTO_ORANGE", "true"), "false")
	if cf != nil && customDomInfo != nil && !customDomInfo.IsCertificateValid() && autoOrange && ctx.Err() == nil {
		if cfDNSProvisioned || customDomInfo.RecordsVerified {
			log.Printf("[Railway] Waiting for Railway ACME SSL validation on %s before enabling Cloudflare Orange Cloud...", customDomInfo.Domain)
			if rw != nil && rw.IsConfigured() {
				updatedInfo, valid := rw.WaitCustomDomainCertificate(ctx, customDomInfo.ID, customDomInfo.Domain, defaultRWCertPollWait)
				if valid && updatedInfo != nil {
					cf.PromoteCustomDomainToOrangeCloud(ctx, updatedInfo.Domain, updatedInfo.CNAMETarget, cnameRecordID)
				} else if ctx.Err() == nil {
					go watchAndPromoteCustomDomain(ctx, sup, rw, cf, customDomInfo, cnameRecordID)
				}
			} else {
				go watchAndPromoteCustomDomain(ctx, sup, nil, cf, customDomInfo, cnameRecordID)
			}
		} else if cf.GetZoneID() != "" {
			go watchAndPromoteCustomDomain(ctx, sup, rw, cf, customDomInfo, cnameRecordID)
		}
	}

	if rw != nil && rw.IsConfigured() && ctx.Err() == nil {
		rw.TuneComputeAndNetwork(ctx)
	}

	if ctx.Err() == nil {
		log.Printf("[Runtime] Synchronized Topology: RailwayDomain=%q | CloudflareDomain=%q | Region=%q",
			railwayDomain, customDomain, region)
		sup.LogProvisioningLink("")
	}
}

func watchAndPromoteCustomDomain(
	parent context.Context,
	sup *Supervisor,
	rw *RailwayClient,
	cf *CloudflareClient,
	info *RailwayCustomDomainInfo,
	cnameRecordID string,
) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if rw != nil && rw.IsConfigured() {
				latest, err := rw.FetchCustomDomainStatus(ctx, info.ID, info.Domain)
				if err == nil && latest != nil && latest.IsCertificateValid() {
					log.Printf("[Railway] ✓ Deferred ACME SSL validation completed for %s (%s)",
						latest.Domain, latest.CertificateStatus)
					cf.PromoteCustomDomainToOrangeCloud(ctx, latest.Domain, latest.CNAMETarget, cnameRecordID)
					if sup != nil {
						sup.LogProvisioningLink("")
					}
					return
				}
			} else if probeCustomDomainTLS(ctx, info.Domain, info.CNAMETarget) {
				log.Printf("[Railway] ✓ Direct Origin TLS + Route Identity verified for %s", info.Domain)
				cf.PromoteCustomDomainToOrangeCloud(ctx, info.Domain, info.CNAMETarget, cnameRecordID)
				if sup != nil {
					sup.LogProvisioningLink("")
				}
				return
			}
		}
	}
}

func bindDualListeners(listenCfg *net.ListenConfig, primaryPort string) ([]net.Listener, []string, error) {
	strictMode := strings.EqualFold(getEnv("BERMUDA_STRICT_PORT", "false"), "true")

	seenPorts := make(map[string]struct{}, 4)
	var portsToTry []string
	addPort := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, exists := seenPorts[p]; !exists {
			seenPorts[p] = struct{}{}
			portsToTry = append(portsToTry, p)
		}
	}

	addPort(primaryPort)
	if !strictMode {
		addPort(defaultPort)
		addPort(compatHttpPort)
		addPort("3000")
	}

	var listeners []net.Listener
	var boundAddrs []string
	var firstErr error

	for i, p := range portsToTry {
		addr := "0.0.0.0:" + p
		ln, err := listenCfg.Listen(context.Background(), "tcp", addr)
		if err != nil {
			if i == 0 {
				firstErr = err
				if strictMode {
					return nil, nil, err
				}
				log.Printf("[Gateway] Notice: Primary port :%s unavailable (%v); binding compatibility ports...", p, err)
			}
			continue
		}
		listeners = append(listeners, ln)
		boundAddrs = append(boundAddrs, ":"+p)
	}

	if len(listeners) == 0 {
		return nil, nil, firstErr
	}
	return listeners, boundAddrs, nil
}

func teardown(
	srv *http.Server,
	gw *Gateway,
	sup *Supervisor,
	supCancel context.CancelFunc,
	cloudCancel context.CancelFunc,
	graceful bool,
) {
	hardDeadline := time.Now().Add(22 * time.Second)

	if cloudCancel != nil {
		cloudCancel()
	}

	gw.SetDraining()
	if graceful {
		log.Println("[Gateway] Stage 1/5: Health status set to 503 (traffic shedding)...")
		time.Sleep(drainPropagationWindow)
	}

	if graceful {
		log.Println("[Gateway] Stage 2/5: Draining HTTP listeners and active XHTTP streams...")
		httpDeadline := minTime(hardDeadline.Add(-7*time.Second), time.Now().Add(11*time.Second))
		httpCtx, cancelHTTP := context.WithDeadline(context.Background(), httpDeadline)
		if err := srv.Shutdown(httpCtx); err != nil {
			log.Printf("[Gateway] HTTP drain timeout (%v); closing listeners", err)
			_ = srv.Close()
		} else {
			log.Println("[Gateway] HTTP listeners drained cleanly")
		}
		cancelHTTP()

		log.Println("[Gateway] Stage 3/5: Waiting for active upgraded WebSocket tunnels...")
		tunnelDeadline := minTime(hardDeadline.Add(-5*time.Second), time.Now().Add(httpDrainTimeout))
		if tunnelDeadline.After(time.Now()) {
			tunnelCtx, cancelTunnel := context.WithDeadline(context.Background(), tunnelDeadline)
			gw.WaitTunnels(tunnelCtx)
			cancelTunnel()
		}
	} else {
		_ = srv.Close()
	}

	log.Println("[Gateway] Stage 4/5: Closing remaining sockets and idle pools...")
	gw.CloseTunnels()
	gw.CloseIdleBackendConns()

	log.Println("[Gateway] Stage 5/5: Stopping worker process group...")
	supCancel()
	remainingSupTime := time.Until(hardDeadline)
	if remainingSupTime <= 0 {
		remainingSupTime = 2 * time.Second
	}
	sup.Stop(remainingSupTime)
}

func main() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Println("[Gateway] Initializing BERMUDA PORTAL Edge Router (Railway Cloud + Cloudflare Edition)...")

	gwMB, workerMB := deriveMemoryBudget()
	procs := applyGOMAXPROCS()

	_ = syscall.Setenv("BERMUDA_WORKER_MEM_MB", strconv.Itoa(workerMB))
	_ = syscall.Setenv("BERMUDA_XRAY_MEM_MB", strconv.Itoa(workerMB))
	_ = syscall.Setenv("BERMUDA_WORKER_GOMAXPROCS", strconv.Itoa(procs))
	_ = syscall.Setenv("BERMUDA_XRAY_GOMAXPROCS", strconv.Itoa(procs))
	log.Printf("[Runtime] Resource budget calibrated: Gateway=%dMiB, Worker=%dMiB (GOGC=100) | GOMAXPROCS=%d",
		gwMB, workerMB, procs)

	sigCtx, stopSig := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSig()

	port := getEnv("BERMUDA_LISTEN_PORT", getEnv("PORT", defaultPort))
	railwayDomain, customDomain := resolveInfrastructureDomains()

	rawRegion := getEnv("BERMUDA_REGION", getEnv("RAILWAY_REPLICA_REGION", getEnv("RAILWAY_REGION", getEnv("REGION", ""))))
	region := normalizeRegion(rawRegion)

	sup := NewSupervisor()

	cfToken := sanitizeCloudflareToken(getEnv("CLOUDFLARE_API_TOKEN", getEnv("BERMUDA_CF_API_TOKEN", "")))
	cfZoneID := sanitizeExplicitCredential(getEnv("CLOUDFLARE_ZONE_ID", getEnv("BERMUDA_CF_ZONE_ID", "")))
	var cf *CloudflareClient
	if cfToken != "" {
		cf = NewCloudflareClient(cfToken, cfZoneID)
		if railwayDomain != "" {
			cf.SetRailwayOrigin(railwayDomain)
		}
		if customDomain != "" {
			cf.SetPreferredDomain(customDomain)
		}
	} else {
		log.Println("[Cloudflare] Notice: CLOUDFLARE_API_TOKEN unset. Edge auto-tuning bypassed.")
	}

	rwToken := sanitizeRailwayToken(getEnv("RAILWAY_API_TOKEN", getEnv("RAILWAY_TOKEN", getEnv("BERMUDA_RAILWAY_TOKEN", ""))))
	var rw *RailwayClient
	if rwToken != "" {
		rw = NewRailwayClient(rwToken)
		if railwayDomain != "" {
			rw.SetServiceDomain(railwayDomain)
		}
		if customDomain != "" {
			rw.SetCustomDomain(customDomain)
		}
	} else {
		log.Println("[Railway] Notice: RAILWAY_API_TOKEN unset. Railway API self-provisioning bypassed.")
	}

	sup.SetInfrastructureDomains(railwayDomain, customDomain, region)
	primaryLogDomain := customDomain
	if primaryLogDomain == "" {
		primaryLogDomain = railwayDomain
	}
	if primaryLogDomain != "" {
		sup.SetPublicDomain(primaryLogDomain)
	}

	log.Printf("[Runtime] Initial Topology: RailwayDomain=%q | CloudflareDomain=%q | Region=%q",
		railwayDomain, customDomain, region)

	gw := NewGateway(sup, cf, railwayDomain, customDomain, region)
	gw.SetRailwayClient(rw)

	topologyUpdater := func(rwDom, cfDom string) {
		sup.SetInfrastructureDomains(rwDom, cfDom, region)
		gw.SetInfrastructureDomains(rwDom, cfDom, region)
	}
	if cf != nil {
		cf.SetTopologyCallback(topologyUpdater)
	}
	if rw != nil {
		rw.SetTopologyCallback(topologyUpdater)
	}

	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       16 * time.Minute,
		MaxHeaderBytes:    32 << 10,
		ConnState:         gw.TrackConnState,
	}

	listenCfg := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     edgeKeepAlivePeriod,
			Interval: edgeKeepAlivePeriod,
			Count:    4,
		},
		Control: func(network, address string, c syscall.RawConn) error {
			_ = c.Control(func(fd uintptr) {
				_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeoutOpt, 90000)
			})
			return nil
		},
	}

	listeners, boundAddrs, err := bindDualListeners(&listenCfg, port)
	if err != nil {
		log.Fatalf("[Gateway] Fatal: Cannot bind HTTP listener on :%s: %v", port, err)
	}

	serverErrCh := make(chan error, len(listeners))
	log.Printf("[Gateway] Edge listeners active on %s (PID %d, GOMAXPROCS=%d)",
		strings.Join(boundAddrs, " & "), os.Getpid(), procs)

	for _, ln := range listeners {
		activeLn := ln
		go func() {
			if err := srv.Serve(activeLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				select {
				case serverErrCh <- err:
				default:
				}
			}
		}()
	}

	supCtx, supCancel := context.WithCancel(context.Background())
	supErrCh := make(chan error, 1)
	go func() {
		supErrCh <- sup.Run(supCtx)
	}()

	go func() {
		select {
		case <-sup.BootstrapDone():
		case <-sigCtx.Done():
			return
		}
		runtime.GC()
		debug.FreeOSMemory()
		applyMemoryCeiling(gwMB)
	}()

	cloudCtx, cloudCancel := context.WithCancel(sigCtx)
	defer cloudCancel()
	if rw != nil || cf != nil {
		go orchestrateCloudInfrastructure(cloudCtx, sup, gw, rw, cf, railwayDomain, customDomain, region)
	}

	select {
	case err := <-serverErrCh:
		log.Printf("[Gateway] Fatal: HTTP server error: %v", err)
		teardown(srv, gw, sup, supCancel, cloudCancel, false)
		os.Exit(1)
	case err := <-supErrCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[Gateway] Fatal: Worker supervisor halted: %v", err)
		}
		teardown(srv, gw, sup, supCancel, cloudCancel, false)
		os.Exit(1)
	case <-sigCtx.Done():
		log.Println("[Gateway] Termination signal received. Initiating graceful teardown...")
	}

	teardown(srv, gw, sup, supCancel, cloudCancel, true)
	log.Println("[Gateway] BERMUDA PORTAL Edge Router shutdown complete. Exit 0.")
}
