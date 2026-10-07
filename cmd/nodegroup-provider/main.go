// nodegroup-provider is the cloud side of cluster-autoscaler (externalgrpc) for RKE1 clusters: it creates VMs
// that join the cluster as workers, and deletes them again.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/bootstrap"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud/vngcloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/provider"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

// version is set at build time (-ldflags -X main.version=...).
var version = "dev"

func main() {
	var (
		configPath     = flag.String("config", "/etc/nodegroup-provider/config.yaml", "provider configuration")
		templatePath   = flag.String("worker-template", "/etc/nodegroup-provider/worker-template.json", "docker inspect of an existing worker's service-sidekick, nginx-proxy, kubelet, kube-proxy")
		bundleDir      = flag.String("node-certs", "/etc/nodegroup-provider/node-certs", "directory with the RKE node certificates (kube-ca.pem, kube-node*.pem, ...)")
		grpcAddr       = flag.String("grpc-listen", ":8086", "gRPC listen address for cluster-autoscaler")
		grpcTLSDir     = flag.String("grpc-tls", "/etc/nodegroup-provider/grpc-tls", "directory with tls.crt, tls.key and ca.crt for mTLS with cluster-autoscaler")
		bootAddr       = flag.String("bootstrap-listen", ":8443", "HTTPS listen address for new VMs")
		bootTLSDir     = flag.String("bootstrap-tls", "/etc/nodegroup-provider/bootstrap-tls", "directory with tls.crt, tls.key and ca.crt of the bootstrap server")
		stateConfigMap = flag.String("state-configmap", "nodegroup-provider-state", "ConfigMap holding the instance records")
		interval       = flag.Duration("reconcile-interval", 30*time.Second, "how often instances are reconciled")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log, *configPath, *templatePath, *bundleDir, *grpcAddr, *grpcTLSDir, *bootAddr, *bootTLSDir, *stateConfigMap, *interval); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath, templatePath, bundleDir, grpcAddr, grpcTLSDir, bootAddr, bootTLSDir, stateCM string, interval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		return errors.New("POD_NAMESPACE must be set")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	tmpl, err := workerplane.Load(templatePath)
	if err != nil {
		return err
	}
	bundle, err := bootstrap.LoadBundle(bundleDir)
	if err != nil {
		return err
	}
	bootCA, err := os.ReadFile(bootTLSDir + "/ca.crt")
	if err != nil {
		return err
	}

	var driver cloud.Driver
	switch cfg.Cloud.Provider {
	case "vngcloud":
		if driver, err = vngcloud.New(ctx, cfg.Cloud.VNGCloud); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported cloud %q", cfg.Cloud.Provider)
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	store := state.New(kube, namespace, stateCM)
	if err := store.Load(ctx); err != nil {
		return err
	}

	p := &provider.Provider{
		Config: cfg, Driver: driver, Store: store, Kube: kube, BootCA: string(bootCA), Log: log,
		Now: time.Now, NameRand: provider.RandomSuffix,
	}

	// gRPC for cluster-autoscaler, mTLS only
	grpcTLS, err := mtlsConfig(grpcTLSDir)
	if err != nil {
		return fmt.Errorf("gRPC TLS: %w", err)
	}
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(grpcTLS)))
	protos.RegisterCloudProviderServer(gs, p)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(gs, hs)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return err
	}

	// HTTPS for new VMs (server TLS only: the VM proves itself with its token)
	bootCert, err := tls.LoadX509KeyPair(bootTLSDir+"/tls.crt", bootTLSDir+"/tls.key")
	if err != nil {
		return fmt.Errorf("bootstrap TLS: %w", err)
	}
	bs := &http.Server{
		Addr:              bootAddr,
		Handler:           (&bootstrap.Server{Config: cfg, Store: store, Template: tmpl, Bundle: bundle, Log: log, Now: time.Now}).Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{bootCert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errc := make(chan error, 2)
	go func() { errc <- gs.Serve(lis) }()
	go func() { errc <- bs.ListenAndServeTLS("", "") }()
	go p.Run(ctx, interval)
	log.Info("nodegroup-provider started", "version", version, "grpc", grpcAddr, "bootstrap", bootAddr, "groups", len(cfg.NodeGroups),
		"cluster", cfg.ClusterName, "kubeletImage", tmpl.KubeletImage())

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = bs.Shutdown(shutdown)
	gs.GracefulStop()
	return nil
}

func mtlsConfig(dir string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(dir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("no CA certificate in ca.crt")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
