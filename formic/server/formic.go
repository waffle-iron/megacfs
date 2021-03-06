package server

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/getcfs/megacfs/formic"
	"github.com/getcfs/megacfs/formic/newproto"
	"github.com/getcfs/megacfs/ftls"
	"github.com/getcfs/megacfs/oort"
	"github.com/gholt/ring"
	"github.com/gholt/store"
	"github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spaolacci/murmur3"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

type Formic struct {
	sync.RWMutex
	waitGroup             *sync.WaitGroup
	shutdownChan          chan struct{}
	started               bool
	comms                 *formic.StoreComms
	fs                    formic.FileService
	grpcServer            *grpc.Server
	validIPs              map[string]map[string]time.Time
	nodeID                int
	grpcAddressIndex      int
	grpcCertFile          string
	grpcKeyFile           string
	caFile                string
	groupGRPCAddressIndex int
	valueGRPCAddressIndex int
	ring                  ring.Ring
	ringPath              string
	logger                *zap.Logger
}

type FormicConfig struct {
	NodeID                int
	GRPCAddressIndex      int
	GroupGRPCAddressIndex int
	ValueGRPCAddressIndex int
	GRPCCertFile          string
	GRPCKeyFile           string
	CAFile                string
	Scale                 float64
	Ring                  ring.Ring
	RingPath              string
	AuthURL               string
	AuthUser              string
	AuthPassword          string
	Logger                *zap.Logger
}

func NewFormicConfig() *FormicConfig {
	// TODO: We can fix this so NodeID: 0 is notset instead of -1
	return &FormicConfig{NodeID: -1}
}

func resolveFormicConfig(c *FormicConfig) *FormicConfig {
	cfg := &FormicConfig{}
	if c != nil {
		*cfg = *c
	}
	if cfg.Logger == nil {
		var err error
		cfg.Logger, err = zap.NewProduction()
		if err != nil {
			panic(err)
		}
	}
	return cfg
}

func NewFormic(cfg *FormicConfig) (*Formic, error) {
	cfg = resolveFormicConfig(cfg)
	f := &Formic{
		waitGroup:             &sync.WaitGroup{},
		validIPs:              make(map[string]map[string]time.Time),
		nodeID:                cfg.NodeID,
		grpcAddressIndex:      cfg.GRPCAddressIndex,
		grpcCertFile:          cfg.GRPCCertFile,
		grpcKeyFile:           cfg.GRPCKeyFile,
		caFile:                cfg.CAFile,
		groupGRPCAddressIndex: cfg.GroupGRPCAddressIndex,
		valueGRPCAddressIndex: cfg.ValueGRPCAddressIndex,
		ring:     cfg.Ring,
		ringPath: cfg.RingPath,
		logger:   cfg.Logger,
	}
	return f, nil
}

func (f *Formic) Startup(ctx context.Context) error {
	f.Lock()
	defer f.Unlock()
	if f.started {
		return nil
	}
	f.started = true
	f.shutdownChan = make(chan struct{})
	f.waitGroup.Add(1)
	go func() {
		mMadeUp := prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "Formic",
			Name:      "MadeUp",
			Help:      "Just a made up metric.",
		})
		prometheus.Register(mMadeUp)
		for {
			select {
			case <-f.shutdownChan:
				f.waitGroup.Done()
				return
			case <-time.After(time.Minute):
				mMadeUp.Add(1)
			}
		}
	}()

	ln := f.ring.LocalNode()
	if ln == nil {
		close(f.shutdownChan)
		return errors.New("no local node set")
	}
	grpcAddr := ln.Address(f.grpcAddressIndex)
	if grpcAddr == "" {
		close(f.shutdownChan)
		return fmt.Errorf("no local node address index %d", f.grpcAddressIndex)
	}
	grpcHostPort, err := ring.CanonicalHostPort(grpcAddr, 12300)
	if err != nil {
		close(f.shutdownChan)
		return err
	}
	// TODO: Eventually get rid of the + 1
	if grpcHostPort[len(grpcHostPort)-1] != '1' {
		grpcHostPort = grpcHostPort[:len(grpcHostPort)-1] + "1"
	}

	groupStore := oort.NewReplGroupStore(&oort.GroupStoreConfig{
		AddressIndex:    f.groupGRPCAddressIndex,
		StoreFTLSConfig: ftls.DefaultClientFTLSConf(f.grpcCertFile, f.grpcKeyFile, f.caFile),
		RingClientID:    grpcHostPort,
		RingCachePath:   f.ringPath,
		Logger:          f.logger,
	})
	valueStore := oort.NewReplValueStore(&oort.ValueStoreConfig{
		AddressIndex:    f.valueGRPCAddressIndex,
		StoreFTLSConfig: ftls.DefaultClientFTLSConf(f.grpcCertFile, f.grpcKeyFile, f.caFile),
		RingClientID:    grpcHostPort,
		RingCachePath:   f.ringPath,
		Logger:          f.logger,
	})
	f.comms, err = formic.NewStoreComms(valueStore, groupStore, f.logger)
	if err != nil {
		close(f.shutdownChan)
		return err
	}
	// TODO: Make sure there are ways to shut this stuff down gracefully.
	deleteChan := make(chan *formic.DeleteItem, 1000)
	dirtyChan := make(chan *formic.DirtyItem, 1000)
	blocksize := int64(1024 * 64) // Default Block Size (64K)
	f.fs = formic.NewOortFS(f.comms, f.logger, deleteChan, dirtyChan, blocksize)
	deletes := formic.NewDeletinator(deleteChan, f.fs, f.comms, f.logger)
	cleaner := formic.NewCleaninator(dirtyChan, f.fs, f.comms, f.logger)
	go deletes.Run()
	go cleaner.Run()

	f.logger.Debug("Listen on", zap.String("grpcHostPort", grpcHostPort))
	lis, err := net.Listen("tcp", grpcHostPort)
	if err != nil {
		close(f.shutdownChan)
		return err
	}
	ftlsCfg := ftls.DefaultServerFTLSConf(f.grpcCertFile, f.grpcKeyFile, f.caFile)
	ftlsCfg.MutualTLS = false // TODO: Currently no way to allow full cert validation
	ftlsCfg.InsecureSkipVerify = true
	tlsCfg, err := ftls.NewServerTLSConfig(ftlsCfg)
	if err != nil {
		close(f.shutdownChan)
		return err
	}
	if f.nodeID == -1 {
		f.nodeID = int(murmur3.Sum32([]byte(grpcHostPort)))
	}
	f.grpcServer = grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)
	newproto.RegisterFormicServer(f.grpcServer, f)
	grpc_prometheus.Register(f.grpcServer)
	f.waitGroup.Add(1)
	go func() {
		err := f.grpcServer.Serve(lis)
		if err != nil {
			f.logger.Debug("grpcServer.Serve error", zap.Error(err))
		}
		lis.Close()
		f.waitGroup.Done()
	}()
	f.waitGroup.Add(1)
	go func() {
		<-f.shutdownChan
		f.grpcServer.Stop()
		lis.Close()
		f.waitGroup.Done()
	}()
	return nil
}

func (f *Formic) Shutdown(ctx context.Context) error {
	f.Lock()
	defer f.Unlock()
	if !f.started {
		return nil
	}
	close(f.shutdownChan)
	f.waitGroup.Wait()
	return nil
}

func (f *Formic) GetAttr(stream newproto.Formic_GetAttrServer) error {
	// NOTE: Each of these streams is synchronized req1, resp1, req2, resp2.
	// But it doesn't have to be that way, it was just simpler to code. Each
	// client/server pair will have a stream for each request/response type, so
	// there's a pretty good amount of concurrency going on there already.
	// Perhaps later we can experiment with intrastream concurrency and see if
	// the complexity is worth it.
	//
	// The main reason for using streams over unary grpc requests was
	// benchmarked speed gains. I suspect it is because unary requests actually
	// set up and tear down streams for each request, but that's just a guess.
	// We stopped looking into it once we noticed the speed gains from
	// switching to streaming.
	var resp newproto.GetAttrResponse
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp.Reset()
		if err = f.validateIP(stream.Context()); err != nil {
			resp.Err = err.Error()
		} else if err = f.fs.NewGetAttr(stream.Context(), req, &resp); err != nil {
			resp.Err = err.Error()
		}
		resp.Rpcid = req.Rpcid
		if err := stream.Send(&resp); err != nil {
			return err
		}
	}
}

func (f *Formic) Read(stream newproto.Formic_ReadServer) error {
	var resp newproto.ReadResponse
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp.Reset()
		if err = f.validateIP(stream.Context()); err != nil {
			resp.Err = err.Error()
		} else if err = f.fs.NewRead(stream.Context(), req, &resp); err != nil {
			resp.Err = err.Error()
		}
		resp.Rpcid = req.Rpcid
		if err := stream.Send(&resp); err != nil {
			return err
		}
	}
}

func (f *Formic) SetAttr(stream newproto.Formic_SetAttrServer) error {
	var resp newproto.SetAttrResponse
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp.Reset()
		if err = f.validateIP(stream.Context()); err != nil {
			resp.Err = err.Error()
		} else if err = f.fs.NewSetAttr(stream.Context(), req, &resp); err != nil {
			resp.Err = err.Error()
		}
		resp.Rpcid = req.Rpcid
		if err := stream.Send(&resp); err != nil {
			return err
		}
	}
}

func (f *Formic) Write(stream newproto.Formic_WriteServer) error {
	var resp newproto.WriteResponse
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		resp.Reset()
		if err = f.validateIP(stream.Context()); err != nil {
			resp.Err = err.Error()
		} else if err = f.fs.NewWrite(stream.Context(), req, &resp); err != nil {
			resp.Err = err.Error()
		}
		resp.Rpcid = req.Rpcid
		if err := stream.Send(&resp); err != nil {
			return err
		}
	}
}

func (f *Formic) validateIP(ctx context.Context) error {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return errors.New("couldn't get client ip")
	}
	ip, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return err
	}
	fsidUUID, err := formic.GetFsId(ctx)
	fsid := fsidUUID.String()
	if err != nil {
		return err
	}
	ips, ok := f.validIPs[fsid]
	if !ok {
		ips = make(map[string]time.Time)
		f.validIPs[fsid] = ips
	}
	cacheTime, ok := ips[ip]
	if ok && cacheTime.After(time.Now()) {
		return nil
	}
	_, err = f.comms.ReadGroupItem(ctx, []byte(fmt.Sprintf("/fs/%s/addr", fsid)), []byte(ip))
	if store.IsNotFound(err) {
		f.logger.Debug("Unauthorized IP", zap.String("unauthorized_ip", ip))
		return formic.ErrUnauthorized
	}
	if err != nil {
		return err
	}
	f.validIPs[fsid][ip] = time.Now().Add(time.Second * time.Duration(180.0+180.0*rand.NormFloat64()*0.1))
	return nil
}
