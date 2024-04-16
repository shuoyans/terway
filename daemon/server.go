package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // import pprof for diagnose
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/AliyunContainerService/terway/pkg/logger"
	"github.com/AliyunContainerService/terway/pkg/metric"
	"github.com/AliyunContainerService/terway/pkg/tracing"
	"github.com/AliyunContainerService/terway/pkg/utils"
	"github.com/AliyunContainerService/terway/rpc"
	"github.com/AliyunContainerService/terway/types"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

const daemonRPCTimeout = 118 * time.Second

// stackTriger print golang stack trace to log
func stackTriger() {
	sigchain := make(chan os.Signal, 1)
	go func(c chan os.Signal) {
		for {
			<-sigchain
			var (
				buf       []byte
				stackSize int
			)
			bufferLen := 16384
			for stackSize == len(buf) {
				buf = make([]byte, bufferLen)
				stackSize = runtime.Stack(buf, true)
				bufferLen *= 2
			}
			buf = buf[:stackSize]
			logger.DefaultLogger.Printf("dump stacks: %s\n", string(buf))
		}
	}(sigchain)

	signal.Notify(sigchain, stackTriggerSignals...)
}

// Run terway daemon
func Run(ctx context.Context, socketFilePath, debugSocketListen, configFilePath, daemonMode string) error {
	err := os.MkdirAll(filepath.Dir(socketFilePath), 0700)
	if err != nil {
		return fmt.Errorf("error create socket dir: %s, %w", filepath.Dir(socketFilePath), err)
	}
	err = syscall.Unlink(socketFilePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("error unlink socket file: %s, %w", socketFilePath, err)
	}
	mask := syscallUmask(0777)
	defer syscallUmask(mask)

	l, err := net.Listen("unix", socketFilePath)
	if err != nil {
		return fmt.Errorf("error listen at %s: %v", socketFilePath, err)
	}

	svc, err := newNetworkService(ctx, configFilePath, daemonMode)
	if err != nil {
		return err
	}

	grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(
		cniInterceptor,
	))
	rpc.RegisterTerwayBackendServer(grpcServer, svc)
	rpc.RegisterTerwayTracingServer(grpcServer, tracing.DefaultRPCServer())

	stop := make(chan struct{})

	stackTriger()
	err = runDebugServer(debugSocketListen)
	if err != nil {
		return err
	}

	go func() {
		serviceLog.Info("start serving", "path", socketFilePath)
		err = grpcServer.Serve(l)
		if err != nil {
			logger.DefaultLogger.Errorf("error start grpc server: %v", err)
			close(stop)
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Minute)
		// defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
			case <-ticker.C:
				err := detectVportGoupLeak()
				if err != nil {
					logger.DefaultLogger.Errorf("error check vport group: %v", err)
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
	case <-stop:
	}
	grpcServer.Stop()

	svc.wg.Wait()

	return nil
}

func runDebugServer(debugSocketListen string) error {
	var (
		l   net.Listener
		err error
	)
	if strings.HasPrefix(debugSocketListen, "unix://") {
		debugSocketListen = strings.TrimPrefix(debugSocketListen, "unix://")
		if err := os.MkdirAll(filepath.Dir(debugSocketListen), 0700); err != nil {
			return err
		}

		if err := syscall.Unlink(debugSocketListen); err != nil && !os.IsNotExist(err) {
			return err
		}

		l, err = net.Listen("unix", debugSocketListen)
		if err != nil {
			return fmt.Errorf("error listen at %s: %v", debugSocketListen, err)
		}
	} else {
		l, err = net.Listen("tcp", debugSocketListen)
		if err != nil {
			return fmt.Errorf("error listen at %s: %v", debugSocketListen, err)
		}
	}

	registerPrometheus()
	http.DefaultServeMux.Handle("/metrics", promhttp.Handler())

	go func() {
		err := http.Serve(l, http.DefaultServeMux)
		if err != nil {
			logger.DefaultLogger.Errorf("error start debug server: %v", err)
		}
	}()

	return nil
}

// RegisterPrometheus register metrics to prometheus server
func registerPrometheus() {
	prometheus.MustRegister(metric.RPCLatency)
	prometheus.MustRegister(metric.OpenAPILatency)
	prometheus.MustRegister(metric.MetadataLatency)
	// ResourcePool
	prometheus.MustRegister(metric.ResourcePoolTotal)
	prometheus.MustRegister(metric.ResourcePoolIdle)
	prometheus.MustRegister(metric.ResourcePoolDisposed)
	// ENIIP
	prometheus.MustRegister(metric.ENIIPFactoryIPCount)
	prometheus.MustRegister(metric.ENIIPFactoryENICount)
	prometheus.MustRegister(metric.ENIIPFactoryIPAllocCount)
}

func cniInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonRPCTimeout)
	defer cancel()

	switch r := req.(type) {
	case *rpc.AllocIPRequest:
		l := logf.FromContext(ctx, "pod", utils.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	case *rpc.ReleaseIPRequest:
		l := logf.FromContext(ctx, "pod", utils.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	case *rpc.GetInfoRequest:
		l := logf.FromContext(ctx, "pod", utils.PodInfoKey(r.K8SPodNamespace, r.K8SPodName), "containerID", r.K8SPodInfraContainerId)
		ctx = logr.NewContext(ctx, l)
	default:
	}
	return handler(ctx, req)
}

func detectVportGoupLeak() error {
	cmd := "vswctl list_vport_group | awk '/^asi-/{sub(/^asi-/, \"\"); print}' | xargs -n1 -I {} bash -c \"crictl pods -o yaml | grep -q {} || echo vport group leaked: {}\""
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		return err
	}
	output := string(out)
	if len(output) > 0 {
		event_msg := "vm eni leak detected:"
		lines := strings.Split(strings.TrimSpace(output), "\n")
		for _, line := range lines {
			event_msg += fmt.Sprintf(" [%s]", line)
		}
		tracing.RecordNodeEvent(corev1.EventTypeWarning, string(types.ErrVmEniLeak), event_msg)
	} else {
		event_msg := "no vm eni leak"
		tracing.RecordNodeEvent(corev1.EventTypeNormal, "periodical eni inspection", event_msg)
	}
	return nil
}
