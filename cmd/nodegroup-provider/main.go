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
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud/ucloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/cloud/vngcloud"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/config"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/preflight"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/protos"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/provider"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/state"
	"github.com/0xphuong/rke-nodegroup-autoscaler/internal/workerplane"
)

// version is set at build time (-ldflags -X main.version=...).
var version = "dev"

func main() {
	// init container mode: copy the RKE node certificates mounted from the host into the pod (see the chart's
	// nodeCerts.hostPath)
	if len(os.Args) > 1 && os.Args[1] == "copy-node-certs" {
		fs := flag.NewFlagSet("copy-node-certs", flag.ExitOnError)
		from := fs.String("from", "/host-node-certs", "directory with the certificate files mounted from the node")
		to := fs.String("to", "/etc/nodegroup-provider/node-certs", "directory the provider reads them from")
		_ = fs.Parse(os.Args[2:])
		if err := bootstrap.CopyBundle(*from, *to); err != nil {
			fmt.Fprintln(os.Stderr, "copy-node-certs:", err)
			os.Exit(1)
		}
		fmt.Printf("copied %d node certificate files from %s to %s\n", len(bootstrap.CertFiles), *from, *to)
		return
	}
	// init container mode: `docker inspect` the worker plane containers of the control plane node the provider
	// runs on, so that no worker template has to be taken by hand (see the chart's workerTemplate)
	if len(os.Args) > 1 && os.Args[1] == "inspect-control-plane" {
		fs := flag.NewFlagSet("inspect-control-plane", flag.ExitOnError)
		socket := fs.String("docker-socket", "/var/run/docker.sock", "Docker Engine API socket of the node")
		out := fs.String("out", "/etc/nodegroup-provider/control-plane/inspect.json", "file the provider reads")
		wait := fs.Duration("wait", 10*time.Minute, "how long to wait for the containers, e.g. while `rke up` recreates them")
		_ = fs.Parse(os.Args[2:])
		if err := inspectControlPlane(*socket, *out, *wait); err != nil {
			fmt.Fprintln(os.Stderr, "inspect-control-plane:", err)
			os.Exit(1)
		}
		fmt.Printf("wrote docker inspect of %v to %s\n", workerplane.ControlPlaneContainers, *out)
		return
	}

	var (
		configPath     = flag.String("config", "/etc/nodegroup-provider/config.yaml", "provider configuration")
		templatePath   = flag.String("worker-template", "", "docker inspect of an existing worker's service-sidekick, nginx-proxy, kubelet, kube-proxy")
		cpInspectPath  = flag.String("control-plane-inspect", "", "instead of -worker-template: docker inspect of a control plane node's service-sidekick, kubelet, kube-proxy (written by inspect-control-plane)")
		bundleDir      = flag.String("node-certs", "/etc/nodegroup-provider/node-certs", "directory with the RKE node certificates (kube-ca.pem, kube-node*.pem, ...)")
		grpcAddr       = flag.String("grpc-listen", ":8086", "gRPC listen address for cluster-autoscaler")
		grpcTLSDir     = flag.String("grpc-tls", "/etc/nodegroup-provider/grpc-tls", "directory with tls.crt, tls.key and ca.crt for mTLS with cluster-autoscaler")
		bootAddr       = flag.String("bootstrap-listen", ":8443", "HTTPS listen address for new VMs")
		bootTLSDir     = flag.String("bootstrap-tls", "/etc/nodegroup-provider/bootstrap-tls", "directory with tls.crt, tls.key and ca.crt of the bootstrap server")
		cloudInitDir   = flag.String("cloud-init", "/etc/nodegroup-provider/cloud-init", "optional directory of site cloud-init documents (#cloud-config or #! scripts) run before the join, in name order")
		stateConfigMap = flag.String("state-configmap", "nodegroup-provider-state", "ConfigMap holding the instance records")
		interval       = flag.Duration("reconcile-interval", 30*time.Second, "how often instances are reconciled")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if (*templatePath == "") == (*cpInspectPath == "") {
		log.Error("fatal", "err", "set exactly one of -worker-template and -control-plane-inspect")
		os.Exit(1)
	}
	if err := run(log, *configPath, *templatePath, *cpInspectPath, *bundleDir, *grpcAddr, *grpcTLSDir, *bootAddr, *bootTLSDir, *cloudInitDir, *stateConfigMap, *interval); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, configPath, templatePath, cpInspectPath, bundleDir, grpcAddr, grpcTLSDir, bootAddr, bootTLSDir, cloudInitDir, stateCM string, interval time.Duration) error {
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
	bundle, err := bootstrap.LoadBundle(bundleDir)
	if err != nil {
		return err
	}
	bootCA, err := os.ReadFile(bootTLSDir + "/ca.crt")
	if err != nil {
		return err
	}
	cloudInit, err := bootstrap.LoadParts(cloudInitDir)
	if err != nil {
		return err
	}
	for _, part := range cloudInit {
		log.Info("cloud-init part", "file", part.Filename, "type", part.ContentType, "bytes", len(part.Content))
	}

	var driver cloud.Driver
	switch cfg.Cloud.Provider {
	case "vngcloud":
		if driver, err = vngcloud.New(ctx, cfg.Cloud.VNGCloud); err != nil {
			return err
		}
	case "ucloud":
		if driver, err = ucloud.New(cfg.Cloud.UCloud); err != nil {
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
	tmpl, err := loadTemplate(ctx, kube, templatePath, cpInspectPath)
	if err != nil {
		return err
	}
	// refuse inputs that would produce broken nodes
	if err := preflight.Bundle(bundle, time.Now(), 30*24*time.Hour, func(w string) { log.Warn("node certificates", "warning", w) }); err != nil {
		return fmt.Errorf("node certificates: %w", err)
	}
	if err := preflight.ClusterCA(bundle, restCfg.TLSClientConfig.CAFile); err != nil {
		return err
	}
	if err := preflight.Template(ctx, kube, tmpl); err != nil {
		return err
	}

	store := state.New(kube, namespace, stateCM)
	if err := store.Load(ctx); err != nil {
		return err
	}

	p := &provider.Provider{
		Config: cfg, Driver: driver, Store: store, Kube: kube, BootCA: string(bootCA), CloudInit: cloudInit, Log: log,
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
		"cluster", cfg.ClusterName, "kubeletImage", tmpl.KubeletImage(), "workerTemplate", templateSource(templatePath),
		"cpHosts", tmpl.CPHosts())

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

// loadTemplate reads the worker template given by hand, or builds it from the control plane node's containers.
func loadTemplate(ctx context.Context, kube kubernetes.Interface, templatePath, cpInspectPath string) (*workerplane.Template, error) {
	if templatePath != "" {
		return workerplane.Load(templatePath)
	}
	b, err := os.ReadFile(cpInspectPath)
	if err != nil {
		return nil, err
	}
	cps, err := preflight.ControlPlaneIPs(ctx, kube)
	if err != nil {
		return nil, fmt.Errorf("list control plane nodes: %w", err)
	}
	return workerplane.FromControlPlane(b, cps)
}

// inspectControlPlane writes the control plane's worker plane containers to out, retrying until they all exist
// and run: the pod may start while `rke up` is recreating them.
func inspectControlPlane(socket, out string, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		b, err := workerplane.InspectLocal(ctx, socket, workerplane.ControlPlaneContainers)
		if err == nil {
			return os.WriteFile(out, b, 0o644)
		}
		fmt.Fprintln(os.Stderr, "inspect-control-plane: retrying:", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(10 * time.Second):
		}
	}
}

func templateSource(templatePath string) string {
	if templatePath != "" {
		return templatePath
	}
	return "control plane node"
}
