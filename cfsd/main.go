package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getcfs/megacfs/formic"
	"github.com/getcfs/megacfs/oort/api/server"
	"github.com/gholt/ring"
	"github.com/uber-go/zap"
	"golang.org/x/net/context"
)

const (
	ADDR_FORMIC = iota
	ADDR_GROUP_GRPC
	ADDR_GROUP_REPL
	ADDR_VALUE_GRPC
	ADDR_VALUE_REPL
)

func main() {
	var ipAddr string
	ringPath := "/etc/cfsd/cfs.ring"
	caPath := "/etc/cfsd/ca.pem"
	var certPath string
	var keyPath string
	dataPath := "/var/lib/cfsd"

	baseLogger := zap.New(zap.NewJSONEncoder())
	baseLogger.SetLevel(zap.InfoLevel)
	logger := baseLogger.With(zap.String("name", "cfsd"))

	fp, err := os.Open(ringPath)
	if err != nil {
		logger.Fatal("Couldn't open ring file", zap.Error(err))
	}
	oneRing, err := ring.LoadRing(fp)
	if err != nil {
		logger.Fatal("Error loading ring", zap.Error(err))
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		logger.Fatal("Couldn't find network interfaces", zap.Error(err))
	}
FIND_LOCAL_NODE:
	for _, addrObj := range addrs {
		if ipNet, ok := addrObj.(*net.IPNet); ok {
			for _, node := range oneRing.Nodes() {
				for _, nodeAddr := range node.Addresses() {
					i := strings.LastIndex(nodeAddr, ":")
					if i < 0 {
						continue
					}
					nodeIP := net.ParseIP(nodeAddr[:i])
					if ipNet.IP.Equal(nodeIP) {
						oneRing.SetLocalNode(node.ID())
						ipAddr = nodeIP.String()
						break FIND_LOCAL_NODE
					}
				}
			}
		}
	}
	certPath = "/etc/cfsd/" + ipAddr + ".pem"
	keyPath = "/etc/cfsd/" + ipAddr + "-key.pem"

	waitGroup := &sync.WaitGroup{}
	shutdownChan := make(chan struct{})

	groupStore, groupStoreRestartChan, err := server.NewGroupStore(&server.GroupStoreConfig{
		GRPCAddressIndex: ADDR_GROUP_GRPC,
		ReplAddressIndex: ADDR_GROUP_REPL,
		CertFile:         certPath,
		KeyFile:          keyPath,
		CAFile:           caPath,
		Scale:            0.4,
		Path:             dataPath,
		Ring:             oneRing,
	})
	if err != nil {
		logger.Fatal("Error initializing group store", zap.Error(err))
	}
	waitGroup.Add(1)
	go func() {
		for {
			select {
			case <-groupStoreRestartChan:
				ctx, _ := context.WithTimeout(context.Background(), time.Minute)
				groupStore.Shutdown(ctx)
				ctx, _ = context.WithTimeout(context.Background(), time.Minute)
				groupStore.Startup(ctx)
			case <-shutdownChan:
				ctx, _ := context.WithTimeout(context.Background(), time.Minute)
				groupStore.Shutdown(ctx)
				waitGroup.Done()
				return
			}
		}
	}()
	ctx, _ := context.WithTimeout(context.Background(), time.Minute)
	if err = groupStore.Startup(ctx); err != nil {
		ctx, _ = context.WithTimeout(context.Background(), time.Minute)
		groupStore.Shutdown(ctx)
		logger.Fatal("Error starting group store", zap.Error(err))
	}

	valueStore, valueStoreRestartChan, err := server.NewValueStore(&server.ValueStoreConfig{
		GRPCAddressIndex: ADDR_VALUE_GRPC,
		ReplAddressIndex: ADDR_VALUE_REPL,
		CertFile:         certPath,
		KeyFile:          keyPath,
		CAFile:           caPath,
		Scale:            0.4,
		Path:             dataPath,
		Ring:             oneRing,
	})
	if err != nil {
		logger.Fatal("Error initializing value store", zap.Error(err))
	}
	waitGroup.Add(1)
	go func() {
		for {
			select {
			case <-valueStoreRestartChan:
				ctx, _ := context.WithTimeout(context.Background(), time.Minute)
				valueStore.Shutdown(ctx)
				ctx, _ = context.WithTimeout(context.Background(), time.Minute)
				valueStore.Startup(ctx)
			case <-shutdownChan:
				ctx, _ := context.WithTimeout(context.Background(), time.Minute)
				valueStore.Shutdown(ctx)
				waitGroup.Done()
				return
			}
		}
	}()
	ctx, _ = context.WithTimeout(context.Background(), time.Minute)
	if err = valueStore.Startup(ctx); err != nil {
		ctx, _ = context.WithTimeout(context.Background(), time.Minute)
		valueStore.Shutdown(ctx)
		logger.Fatal("Error starting value store", zap.Error(err))
	}

	// Startup formic
	err = formic.NewFormicServer(&formic.Config{
		FormicAddressIndex: ADDR_FORMIC,
		ValueAddressIndex:  ADDR_VALUE_GRPC,
		GroupAddressIndex:  ADDR_GROUP_GRPC,
		CertFile:           certPath,
		KeyFile:            keyPath,
		CAFile:             caPath,
		Ring:               oneRing,
		RingPath:           ringPath,
		IpAddr:             ipAddr,
		AuthUrl:            "http://localhost:5000",
		AuthUser:           "admin",
		AuthPassword:       "admin",
	}, logger)
	if err != nil {
		logger.Fatal("Couldn't start up formic", zap.Error(err))
	}

	ch := make(chan os.Signal)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	waitGroup.Add(1)
	go func() {
		for {
			select {
			case <-ch:
				fmt.Println("Shutting down due to signal")
				close(shutdownChan)
				waitGroup.Done()
				return
			case <-shutdownChan:
				waitGroup.Done()
				return
			}
		}
	}()

	fmt.Println("Done launching components")
	waitGroup.Wait()
}
