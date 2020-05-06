// Copyright 2014 The roc Author. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package rocserv

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"sync"
	"syscall"

	"gitlab.pri.ibanyu.com/middleware/seaweed/xconfig"
	stat "gitlab.pri.ibanyu.com/middleware/seaweed/xstat/sys"
	xprom "gitlab.pri.ibanyu.com/middleware/seaweed/xstat/xmetric/xprometheus"

	"git.apache.org/thrift.git/lib/go/thrift"
	"github.com/gin-gonic/gin"
	"github.com/julienschmidt/httprouter"
	"github.com/shawnfeng/sutil/slog"
	"github.com/shawnfeng/sutil/slog/statlog"
	"github.com/shawnfeng/sutil/trace"
)

const (
	PROCESSOR_HTTP   = "http"
	PROCESSOR_THRIFT = "thrift"
	PROCESSOR_GRPC   = "gprc"
	PROCESSOR_GIN    = "gin"

	MODEL_SERVER      = 0
	MODEL_MASTERSLAVE = 1
)

var server = NewServer()

// Server ...
type Server struct {
	sbase ServBase

	mutex   sync.Mutex
	servers map[string]interface{}
}

// NewServer create new server
func NewServer() *Server {
	return &Server{
		servers: make(map[string]interface{}),
	}
}

type cmdArgs struct {
	logMaxSize    int
	logMaxBackups int
	servLoc       string
	logDir        string
	sessKey       string
	sidOffset     int
	group         string
	disable       bool
	model         int
}

func (m *Server) parseFlag() (*cmdArgs, error) {
	var serv, logDir, skey, group string
	var logMaxSize, logMaxBackups, sidOffset int
	flag.IntVar(&logMaxSize, "logmaxsize", 0, "logMaxSize is the maximum size in megabytes of the log file")
	flag.IntVar(&logMaxBackups, "logmaxbackups", 0, "logmaxbackups is the maximum number of old log files to retain")
	flag.StringVar(&serv, "serv", "", "servic name")
	flag.StringVar(&logDir, "logdir", "", "serice log dir")
	flag.StringVar(&skey, "skey", "", "service session key")
	flag.IntVar(&sidOffset, "sidoffset", 0, "service id offset for different data center")
	flag.StringVar(&group, "group", "", "service group")

	flag.Parse()

	if len(serv) == 0 {
		return nil, fmt.Errorf("serv args need!")
	}

	if len(skey) == 0 {
		return nil, fmt.Errorf("skey args need!")
	}

	return &cmdArgs{
		logMaxSize:    logMaxSize,
		logMaxBackups: logMaxBackups,
		servLoc:       serv,
		logDir:        logDir,
		sessKey:       skey,
		sidOffset:     sidOffset,
		group:         group,
	}, nil

}

func (m *Server) loadDriver(sb ServBase, procs map[string]Processor) (map[string]*ServInfo, error) {
	fun := "Server.loadDriver -->"

	infos := make(map[string]*ServInfo)

	for n, p := range procs {
		addr, driver := p.Driver()
		if driver == nil {
			slog.Infof("%s processor:%s no driver", fun, n)
			continue
		}

		slog.Infof("%s processor:%s type:%s addr:%s", fun, n, reflect.TypeOf(driver), addr)

		switch d := driver.(type) {
		case *httprouter.Router:
			sa, err := powerHttp(addr, d)
			if err != nil {
				return nil, err
			}

			slog.Infof("%s load ok processor:%s serv addr:%s", fun, n, sa)
			infos[n] = &ServInfo{
				Type: PROCESSOR_HTTP,
				Addr: sa,
			}

		case thrift.TProcessor:
			sa, err := powerThrift(addr, d)
			if err != nil {
				return nil, err
			}

			slog.Infof("%s load ok processor:%s serv addr:%s", fun, n, sa)
			infos[n] = &ServInfo{
				Type: PROCESSOR_THRIFT,
				Addr: sa,
			}
		case *GrpcServer:
			sa, err := powerGrpc(addr, d)
			if err != nil {
				return nil, err
			}

			slog.Infof("%s load ok processor:%s serv addr:%s", fun, n, sa)
			infos[n] = &ServInfo{
				Type: PROCESSOR_GRPC,
				Addr: sa,
			}
		case *gin.Engine:
			sa, serv, err := powerGin(addr, d)
			if err != nil {
				return nil, err
			}

			m.addServer(n, serv)

			slog.Infof("%s load ok processor:%s serv addr:%s", fun, n, sa)
			infos[n] = &ServInfo{
				Type: PROCESSOR_GIN,
				Addr: sa,
			}
		default:
			return nil, fmt.Errorf("processor:%s driver not recognition", n)

		}
	}

	return infos, nil
}

func (m *Server) addServer(processor string, server interface{}) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.servers[processor] = server
}

func (m *Server) reloadRouter(processor string, driver interface{}) error {
	//fun := "Server.reloadRouter -->"

	m.mutex.Lock()
	defer m.mutex.Unlock()
	server, ok := m.servers[processor]
	if !ok {
		return fmt.Errorf("processor:%s driver not recognition", processor)
	}

	return reloadRouter(processor, server, driver)
}

// Serve handle request and return response
func (m *Server) Serve(confEtcd configEtcd, initfn func(ServBase) error, procs map[string]Processor) error {
	fun := "Server.Serve -->"

	args, err := m.parseFlag()
	if err != nil {
		slog.Panicf("%s parse arg err:%s", fun, err)
		return err
	}

	return m.Init(confEtcd, args, initfn, procs)
}

func (m *Server) initLog(sb *ServBaseV2, args *cmdArgs) error {
	fun := "Server.initLog -->"

	logDir := args.logDir
	var logConfig struct {
		Log struct {
			Level string
			Dir   string
		}
	}
	logConfig.Log.Level = "INFO"

	err := sb.ServConfig(&logConfig)
	if err != nil {
		slog.Errorf("%s serv config err:%s", fun, err)
		return err
	}

	var logdir string
	if len(logConfig.Log.Dir) > 0 {
		logdir = fmt.Sprintf("%s/%s", logConfig.Log.Dir, sb.Copyname())
	}

	if len(logDir) > 0 {
		logdir = fmt.Sprintf("%s/%s", logDir, sb.Copyname())
	}

	if logDir == "console" {
		logdir = ""
	}

	slog.Infof("%s init log dir:%s name:%s level:%s", fun, logdir, args.servLoc, logConfig.Log.Level)

	slog.Init(logdir, "serv.log", logConfig.Log.Level)
	statlog.Init(logdir, "stat.log", args.servLoc)
	return nil
}

func (m *Server) Init(confEtcd configEtcd, args *cmdArgs, initfn func(ServBase) error, procs map[string]Processor) error {
	fun := "Server.Init -->"

	servLoc := args.servLoc
	sessKey := args.sessKey

	sb, err := NewServBaseV2(confEtcd, servLoc, sessKey, args.group, args.sidOffset)
	if err != nil {
		slog.Panicf("%s init servbase loc:%s key:%s err:%s", fun, servLoc, sessKey, err)
		return err
	}
	m.sbase = sb

	// 初始化日志
	m.initLog(sb, args)

	// 初始化服务进程打点
	stat.Init(sb.servGroup, sb.servName, "")

	defer slog.Sync()
	defer statlog.Sync()

	// NOTE: initBackdoor会启动http服务，但由于health check的http请求不需要追踪，且它是判断服务启动与否的关键，所以initTracer可以放在它之后进行
	m.initBackdoor(sb)

	err = m.handleModel(sb, servLoc, args.model)
	if err != nil {
		slog.Panicf("%s handleModel err:%s", fun, err)
		return err
	}

	// App层初始化
	err = initfn(sb)
	if err != nil {
		slog.Panicf("%s callInitFunc err:%s", fun, err)
		return err
	}

	// NOTE: processor 在初始化 trace middleware 前需要保证 xtrace.GlobalTracer() 初始化完毕
	m.initTracer(servLoc)

	err = m.initProcessor(sb, procs)
	if err != nil {
		slog.Panicf("%s initProcessor err:%s", fun, err)
		return err
	}

	sb.SetGroupAndDisable(args.group, args.disable)
	m.initMetric(sb)

	slog.Infoln("server start success...")

	m.awaitSignal(sb)

	return nil
}

func (m *Server) awaitSignal(sb *ServBaseV2) {
	c := make(chan os.Signal, 1)
	signals := []os.Signal{syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGPIPE}
	signal.Reset(signals...)
	signal.Notify(c, signals...)

	for {
		select {
		case s := <-c:
			slog.Infof("receive a signal:%s", s.String())

			if s.String() == syscall.SIGTERM.String() {
				slog.Infof("receive a signal:%s, stop server", s.String())
				sb.Stop()
				<-(chan int)(nil)
			}
		}
	}

}

func (m *Server) handleModel(sb *ServBaseV2, servLoc string, model int) error {
	fun := "Server.handleModel -->"

	if model == MODEL_MASTERSLAVE {
		lockKey := fmt.Sprintf("%s-master-slave", servLoc)
		if err := sb.LockGlobal(lockKey); err != nil {
			slog.Errorf("%s LockGlobal key: %s, err: %s", fun, lockKey, err)
			return err
		}

		slog.Infof("%s LockGlobal succ, key: %s", fun, lockKey)
	}

	return nil
}

func (m *Server) initProcessor(sb *ServBaseV2, procs map[string]Processor) error {
	fun := "Server.initProcessor -->"

	for n, p := range procs {
		if len(n) == 0 {
			slog.Errorf("%s processor name empty", fun)
			return fmt.Errorf("processor name empty")
		}

		if n[0] == '_' {
			slog.Errorf("%s processor name can not prefix '_'", fun)
			return fmt.Errorf("processor name can not prefix '_'")
		}

		if p == nil {
			slog.Errorf("%s processor:%s is nil", fun, n)
			return fmt.Errorf("processor:%s is nil", n)
		} else {
			err := p.Init()
			if err != nil {
				slog.Errorf("%s processor:%s init err:%v", fun, n, err)
				return fmt.Errorf("processor:%s init err:%s", n, err)
			}
		}
	}

	infos, err := m.loadDriver(sb, procs)
	if err != nil {
		slog.Errorf("%s load driver err:%s", fun, err)
		return err
	}

	err = sb.RegisterService(infos)
	if err != nil {
		slog.Errorf("%s register service err:%s", fun, err)
		return err
	}

	// 注册跨机房服务
	err = sb.RegisterCrossDCService(infos)
	if err != nil {
		slog.Errorf("%s register cross dc failed, err: %v", fun, err)
		return err
	}

	return nil
}

func (m *Server) initTracer(servLoc string) error {
	fun := "Server.initTracer -->"

	err := trace.InitDefaultTracer(servLoc)
	if err != nil {
		slog.Errorf("%s init tracer fail:%v", fun, err)
	}

	err = trace.InitTraceSpanFilter()
	if err != nil {
		slog.Errorf("%s init trace span filter fail: %s", fun, err.Error())
	}

	return err
}

func (m *Server) initBackdoor(sb *ServBaseV2) error {
	fun := "Server.initBackdoor -->"

	backdoor := &backDoorHttp{}
	err := backdoor.Init()
	if err != nil {
		slog.Errorf("%s init backdoor err:%s", fun, err)
		return err
	}

	binfos, err := m.loadDriver(sb, map[string]Processor{"_PROC_BACKDOOR": backdoor})
	if err == nil {
		err = sb.RegisterBackDoor(binfos)
		if err != nil {
			slog.Errorf("%s register backdoor err:%s", fun, err)
		}

	} else {
		slog.Warnf("%s load backdoor driver err:%s", fun, err)
	}

	return err
}

func (m *Server) initMetric(sb *ServBaseV2) error {
	fun := "Server.initMetric -->"

	metrics := xprom.NewMetricProcessor()
	err := metrics.Init()
	if err != nil {
		slog.Warnf("%s init metrics err:%s", fun, err)
	}

	metricInfo, err := m.loadDriver(sb, map[string]Processor{"_PROC_METRICS": metrics})
	if err == nil {
		err = sb.RegisterMetrics(metricInfo)
		if err != nil {
			slog.Warnf("%s register backdoor err:%s", fun, err)
		}

	} else {
		slog.Warnf("%s load metrics driver err:%s", fun, err)
	}
	return err
}

func ReloadRouter(processor string, driver interface{}) error {
	return server.reloadRouter(processor, driver)
}

// Serve app call Serve to start server, initLogic is the init func in app, logic.InitLogic,
func Serve(etcdAddrs []string, baseLoc string, initLogic func(ServBase) error, processors map[string]Processor) error {
	return server.Serve(configEtcd{etcdAddrs, baseLoc}, initLogic, processors)
}

// MasterSlave Leader-Follower模式，通过etcd distribute lock进行选举
func MasterSlave(etcdAddrs []string, baseLoc string, initLogic func(ServBase) error, processors map[string]Processor) error {
	return server.MasterSlave(configEtcd{etcdAddrs, baseLoc}, initLogic, processors)
}

func (m *Server) MasterSlave(confEtcd configEtcd, initLogic func(ServBase) error, processors map[string]Processor) error {
	fun := "Server.MasterSlave -->"

	args, err := m.parseFlag()
	if err != nil {
		slog.Panicf("%s parse arg err:%s", fun, err)
		return err
	}
	args.model = MODEL_MASTERSLAVE

	return m.Init(confEtcd, args, initLogic, processors)
}

// Init use in test of application
func Init(etcdAddrs []string, baseLoc string, servLoc, servKey, logDir string, initLogic func(ServBase) error, processors map[string]Processor) error {
	args := &cmdArgs{
		logMaxSize:    0,
		logMaxBackups: 0,
		servLoc:       servLoc,
		logDir:        logDir,
		sessKey:       servKey,
	}
	return server.Init(configEtcd{etcdAddrs, baseLoc}, args, initLogic, processors)
}

func GetServBase() ServBase {
	return server.sbase
}

func GetServName() (servName string) {
	if server.sbase != nil {
		servName = server.sbase.Servname()
	}
	return
}

// GetGroupAndService return group and service name of this service
func GetGroupAndService() (group, service string) {
	serviceKey := GetServName()
	serviceKeyArray := strings.Split(serviceKey, "/")
	if len(serviceKeyArray) == 2 {
		group = serviceKeyArray[0]
		service = serviceKeyArray[1]
	}
	return
}

func GetServId() (servId int) {
	if server.sbase != nil {
		servId = server.sbase.Servid()
	}
	return
}

// GetConfigCenter get serv conf center
func GetConfigCenter() xconfig.ConfigCenter {
	if server.sbase != nil {
		return server.sbase.ConfigCenter()
	}
	return nil
}

// GetAddress get processor ip+port by processorName
func GetProcessorAddress(processorName string) (addr string) {
	if server == nil {
		return
	}
	reginfos := server.sbase.RegInfos()
	for _, val := range reginfos {
		data := new(RegData)
		err := json.Unmarshal([]byte(val), data)
		if err != nil {
			slog.Warnf("GetProcessorAddress unmarshal, val = %s, err = %s", val, err.Error())
			continue
		}
		if servInfo, ok := data.Servs[processorName]; ok {
			addr = servInfo.Addr
			return
		}
	}
	return
}

// Test 方便开发人员在本地启动服务、测试，实例信息不会注册到etcd
func Test(etcdAddrs []string, baseLoc, servLoc string, initLogic func(ServBase) error) error {
	args := &cmdArgs{
		logMaxSize:    0,
		logMaxBackups: 0,
		servLoc:       servLoc,
		sessKey:       "test",
		logDir:        "console",
		disable:       true,
	}
	return server.Init(configEtcd{etcdAddrs, baseLoc}, args, initLogic, nil)
}