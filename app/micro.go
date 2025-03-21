package app

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/holdno/go-instrumentation/conncache"
	etcdregister "github.com/spacegrower/watermelon/infra/register/etcd"
	"github.com/spacegrower/watermelon/infra/resolver/etcd"
	"github.com/spacegrower/watermelon/infra/wlog"
	"github.com/spacegrower/watermelon/pkg/safe"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"

	"github.com/holdno/gopherCron/common"
	"github.com/holdno/gopherCron/errors"
	"github.com/holdno/gopherCron/jwt"
	"github.com/holdno/gopherCron/pkg/cronpb"
	"github.com/holdno/gopherCron/pkg/infra"
	"github.com/holdno/gopherCron/pkg/warning"
	"github.com/holdno/gopherCron/utils"
)

type ConnCacheKey struct {
	Endpoint string
	Region   string
}

func (k ConnCacheKey) String() string {
	return fmt.Sprintf("%s?region=%s", k.Endpoint, k.Region)
}

func installConnCache(a *app) {
	poolSize := 1000
	expire := time.Minute * 30
	poolGauge := a.metrics.NewGaugeVec("connect_cache_pool_size", nil)
	usageGauge := a.metrics.NewGaugeVec("connect_cache_usage", nil)
	expireGauge := a.metrics.NewGaugeVec("connect_cache_expire_duration", nil)
	expirationsTotal := a.metrics.NewCounterVec("connect_cache_expirations_total", []string{"key", "reason"})

	poolGauge.WithLabelValues().Set(float64(poolSize))
	expireGauge.WithLabelValues().Set(float64(expire.Seconds()))

	genMetadata := func(ctx context.Context) context.Context {
		md, exist := metadata.FromOutgoingContext(ctx)
		if !exist {
			md = metadata.New(map[string]string{})
		}
		md.Set(common.GOPHERCRON_AGENT_IP_MD_KEY, a.GetIP())
		return metadata.NewOutgoingContext(ctx, md)
	}

	a.__centerConncets = conncache.NewConnCache[CenterConnCacheKey, *conncache.GRPCConn[CenterConnCacheKey, *grpc.ClientConn]](poolSize, expire,
		func(ctx context.Context, addr CenterConnCacheKey) (*conncache.GRPCConn[CenterConnCacheKey, *grpc.ClientConn], error) {
			gopts := []grpc.DialOption{
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithPerRPCCredentials(a.authenticator),
				grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
					return invoker(genMetadata(ctx), method, req, reply, cc, opts...)
				}),
				grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
					return streamer(genMetadata(ctx), desc, cc, method, opts...)
				}),
			}
			dialAddress := addr.Endpoint

			if addr.Region != a.cfg.Micro.Region {
				dialAddress, gopts = BuildProxyDialerInfo(ctx, addr.Region, addr.Endpoint, gopts)
			}
			cc, err := grpc.DialContext(ctx, dialAddress, gopts...)
			if err != nil {
				return nil, fmt.Errorf("failed to connect center %s, error: %s", addr.Endpoint, err.Error())
			}
			return conncache.WrapGrpcConn[CenterConnCacheKey, *grpc.ClientConn](addr, cc), err
		}, func(_ CenterConnCacheKey) {
			usageGauge.WithLabelValues().Set(float64(a.__centerConncets.Len()))
		}, func(sm CenterConnCacheKey, rr conncache.RemoveReason) {
			usageGauge.WithLabelValues().Set(float64(a.__centerConncets.Len()))
			expirationsTotal.WithLabelValues(sm.Endpoint, rr.Reason()).Inc()
		})
}

func (a *app) RemoveClientRegister(client string) error {
	list, err := a.GetCenterSrvList()
	if err != nil {
		return err
	}

	removed := false

	disposeOne := func(v *CenterClient) (*cronpb.Result, error) {
		defer v.Close()
		ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
		defer cancel()
		resp, err := v.RemoveStream(ctx, &cronpb.RemoveStreamRequest{
			Client: client,
		})
		if err != nil {
			return nil, errors.NewError(http.StatusInternalServerError, "failed to remove stream")
		}
		return resp, nil
	}

	for _, v := range list {
		if strings.Contains(v.addr, a.GetIP()) {
			stream := a.StreamManager().GetStreamsByHost(client)
			if stream != nil {
				stream.Cancel()
				continue
			}
			streamV2 := a.StreamManagerV2().GetStreamsByHost(client)
			if streamV2 != nil {
				streamV2.Cancel()
				continue
			}
		}
		resp, err := disposeOne(v)
		if err != nil {
			return err
		}

		if resp.Result {
			removed = true
			break
		}
	}

	if !removed {
		return errors.NewError(http.StatusInternalServerError, "failed to remove stream: not found")
	}
	return nil
}

type JobDispatcher func(taskRaw []byte) error

func (a *app) DispatchAgentJob(projectID int64, dispatcher JobDispatcher) error {
	mtimer := a.metrics.CustomHistogramSet("dispatch_agent_jobs")
	defer mtimer.ObserveDuration()

	if dispatcher == nil {
		wlog.Error("failed to get grpc streams", zap.Int64("project_id", projectID))
		return fmt.Errorf("failed to dispatch agent jobs, empty streams")
	}

	preKey := common.BuildKey(projectID, "")
	var (
		err     error
		getResp *clientv3.GetResponse
	)
	if err := utils.RetryFunc(5, func() error {
		if getResp, err = a.etcd.KV().Get(context.TODO(), preKey, clientv3.WithPrefix()); err != nil {
			return err
		}
		return nil
	}); err != nil {
		warningErr := a.Warning(warning.NewSystemWarningData(warning.SystemWarning{
			Endpoint: a.GetIP(),
			Type:     warning.SERVICE_TYPE_CENTER,
			Message:  fmt.Sprintf("center-service: %s, etcd kv get error: %s, projectid: %d", a.GetIP(), err.Error(), projectID),
		}))
		if warningErr != nil {
			wlog.Error(fmt.Sprintf("[agent - TaskWatcher] failed to push warning, %s", err.Error()))
		}
		return err
	}

	var tasks [][]byte
	for _, kvPair := range getResp.Kvs {
		if common.IsStatusKey(string(kvPair.Key)) || common.IsAckKey(string(kvPair.Key)) {
			continue
		}
		tasks = append(tasks, kvPair.Value)
	}

	wlog.Info("dispatch agent job", zap.Int64("project_id", projectID), zap.Int("tasks", len(tasks)))

	for _, taskRaw := range tasks {
		if err := dispatcher(taskRaw); err != nil {
			return err
		}
	}

	return nil
}

type AgentClient struct {
	cronpb.AgentClient
	addr   string
	cse    string
	cancel func()
}

func (a *AgentClient) Close() {
	if a.cancel != nil {
		a.cancel()
	}
}

func (a *app) GetAgentClient(region string, projectID int64) (*AgentClient, error) {
	// client 的连接对象由调用时提供初始化
	newConn := infra.NewClientConn()
	cc, err := newConn(cronpb.Agent_ServiceDesc.ServiceName,
		newConn.WithRegion(region),
		newConn.WithSystem(projectID),
		newConn.WithOrg(a.cfg.Micro.OrgID),
		newConn.WithGrpcDialOptions(grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(a.authenticator)),
		newConn.WithServiceResolver(infra.MustSetupEtcdResolver()))
	if err != nil {
		return nil, errors.NewError(http.StatusInternalServerError, fmt.Sprintf("连接agent失败，project_id: %d", projectID)).WithLog(err.Error())
	}

	client := &AgentClient{
		AgentClient: cronpb.NewAgentClient(cc),
		addr:        fmt.Sprintf("resolve_%s_%d", region, projectID),
		cancel: func() {
			cc.Close()
		},
	}

	return client, nil
}

type FinderResult struct {
	addr resolver.Address
	attr infra.NodeMeta
}

func (a *app) GetAgentRegisterMeta(region string, projectID int64, host string) (*FinderResult, error) {
	results, err := a.getAgentAddrs(region, projectID)
	if err != nil {
		return nil, err
	}

	for _, v := range results {
		if v.attr.Host == host || fmt.Sprintf("%s:%d", v.attr.Host, v.attr.Port) == host {
			return v, nil
		}
	}

	return nil, fmt.Errorf("client %s is not found", host)
}

func (a *app) GetAgentStream(ctx context.Context, projectID int64, host string) (*CenterClient, error) {
	// 下发权重变更到对应的host
	meta, err := a.GetAgentRegisterMeta(a.GetConfig().Micro.Region, projectID, host)
	if err != nil {
		return nil, err
	}

	stream, err := a.genCenterStream(ctx, meta.addr.Addr, meta.attr)
	if err != nil {
		return nil, err
	}

	return stream, nil
}

func (a *app) UpdateAgentRegisterWeight(projectID int64, host string, weight int32) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	// 下发权重变更到对应的host
	meta, err := a.GetAgentRegisterMeta(a.GetConfig().Micro.Region, projectID, host)
	if err != nil {
		return err
	}

	stream, err := a.genCenterStream(ctx, meta.addr.Addr, meta.attr)
	if err != nil {
		return err
	}

	resp, err := stream.SendEvent(ctx, &cronpb.SendEventRequest{
		Region:    a.GetConfig().Micro.Region,
		ProjectId: projectID,
		Agent:     meta.addr.Addr,
		Event: &cronpb.ServiceEvent{
			Id:        utils.GetStrID(),
			Type:      cronpb.EventType_EVENT_MODIFY_NODE_META,
			EventTime: time.Now().Unix(),
			Event: &cronpb.ServiceEvent_ModifyNodeMeta{
				ModifyNodeMeta: &cronpb.ModifyNodeRegisterMeta{
					Weight: weight,
				},
			},
		},
	})

	if err != nil {
		wlog.Error("failed to set agent node weight", zap.Error(err))
		return errors.NewError(http.StatusInternalServerError, "failed to set agent node weight").WithLog(err.Error())
	}

	if resp.Type == cronpb.EventType_EVENT_CLIENT_UNSUPPORT {
		return fmt.Errorf("client未支持该事件类型")
	}

	return nil
}

func genAgentRegisterPrefix(projectID int64) string {
	return filepath.ToSlash(filepath.Join(etcdregister.GetETCDPrefixKey(), "gophercron", strconv.FormatInt(projectID, 10), cronpb.Agent_ServiceDesc.ServiceName)) + "/"
}

func genFullAgentRegisterKey(projectID int64, hostAndPort string) string {
	return filepath.ToSlash(filepath.Join(genAgentRegisterPrefix(projectID), "node", hostAndPort))
}

func (a *app) getAgentAddrs(region string, projectID int64) ([]*FinderResult, error) {
	mtimer := a.metrics.CustomHistogramSet("get_agents_list")
	defer mtimer.ObserveDuration()
	finder := etcd.NewFinder[infra.NodeMeta](infra.ResolveEtcdClient())
	ctx, cancel := context.WithTimeout(context.TODO(), time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
	defer cancel()
	addrs, _, err := finder.FindAll(ctx, genAgentRegisterPrefix(projectID))
	if err != nil {
		a.metrics.CustomInc("find_agents_error", fmt.Sprintf("%s_%d", region, projectID), err.Error())
		return nil, err
	}

	var list []*FinderResult
	for _, v := range addrs {
		attr, ok := infra.GetNodeMetaAttribute(v)
		if !ok {
			wlog.Error("failed to get agent node attribute", zap.String("address", v.Addr))
			continue
		}
		list = append(list, &FinderResult{
			addr: v,
			attr: attr,
		})
	}
	return list, nil
}

// ChooseNode 根据权重随机选择一个节点
func ChooseNode(nodes []*FinderResult) *FinderResult {
	if len(nodes) == 0 {
		return nil
	}
	var totalWeight int
	for _, node := range nodes {
		totalWeight += int(node.attr.Weight())
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	randWeight := r.Intn(totalWeight)

	var weightSum int
	for _, node := range nodes {
		weightSum += int(node.attr.Weight())
		if randWeight < weightSum {
			return node
		}
	}

	// 默认返回第一个节点
	return nodes[0]
}

func (a *app) GetAgentStreamRand(ctx context.Context, region string, projectID int64) (*CenterClient, error) {
	// client 的连接对象由调用时提供初始化
	addrs, err := a.getAgentAddrs(region, projectID)
	if err != nil {
		return nil, err
	}

	var filtered []*FinderResult
	for _, item := range addrs {
		if item.attr.CenterServiceEndpoint == "" {
			continue
		}
		filtered = append(filtered, item)
	}

	item := ChooseNode(filtered)
	if item == nil {
		return nil, nil
	}

	client, err := a.genCenterStream(ctx, item.addr.Addr, item.attr)
	if err != nil {
		return nil, err
	}
	return client, nil
}

// genCenterStream 根据agent的meta信息生成基于中心服务的stream连接
func (a *app) genCenterStream(ctx context.Context, agentAddr string, meta infra.NodeMeta) (*CenterClient, error) {
	if meta.CenterServiceEndpoint == "" {
		return nil, fmt.Errorf("center service endpoint is empty")
	}
	dialAddress := meta.CenterServiceEndpoint
	cc, err := a.getCenterConnect(ctx, meta.CenterServiceRegion, dialAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to connect agent stream %s, error: %s", dialAddress, err.Error())
	}

	client := &CenterClient{
		CenterClient: cronpb.NewCenterClient(cc.ClientConn()),
		addr:         agentAddr,
		cancel: func() {
			cc.Done()
		},
	}
	return client, nil
}

// FindAgentsV2 实际拿到的是中心的地址，每个agent有跟某个中心建立长链接
func (a *app) FindAgentsV2(region string, projectID int64) ([]*CenterClient, error) {
	addrs, err := a.getAgentAddrs(region, projectID)
	if err != nil {
		return nil, err
	}
	var (
		list []*CenterClient
	)

	for _, item := range addrs {
		if item.attr.CenterServiceEndpoint == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.TODO(), time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
		defer cancel()
		client, err := a.genCenterStream(ctx, item.addr.Addr, item.attr)
		if err != nil {
			return nil, err
		}
		list = append(list, client)
	}

	return list, nil
}

func (a *app) FindAgents(region string, projectID int64) ([]*AgentClient, error) {
	addrs, err := a.getAgentAddrs(region, projectID)
	if err != nil {
		return nil, err
	}
	var list []*AgentClient
	for _, item := range addrs {
		err := func() error {
			ctx, cancel := context.WithTimeout(context.TODO(), time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
			defer cancel()
			gopts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithPerRPCCredentials(a.authenticator)}
			dialAddress := item.addr.Addr
			if item.attr.Region != a.cfg.Micro.Region {
				dialAddress, gopts = BuildProxyDialerInfo(ctx, item.attr.Region, item.addr.Addr, gopts)
			}
			cc, err := grpc.DialContext(ctx, dialAddress, gopts...)
			if err != nil {
				return err
			}
			client := &AgentClient{
				AgentClient: cronpb.NewAgentClient(cc),
				addr:        item.addr.Addr,
				cancel: func() {
					cc.Close()
				},
			}
			list = append(list, client)
			return nil
		}()
		if err != nil {
			return nil, fmt.Errorf("failed to connect agent %s, error: %s", item.addr.Addr, err.Error())
		}
	}

	return list, nil
}

type CenterClient struct {
	cronpb.CenterClient
	cancel func()
	addr   string
}

func (c *CenterClient) Close() {
	if c.cancel != nil {
		c.cancel()
	}
}

func resolveCenterService(a *app) {
	finder := etcd.NewAsyncFinder[infra.NodeMeta](infra.ResolveEtcdClient(),
		etcd.NewEtcdTarget(a.cfg.Micro.OrgID, "0", cronpb.Center_ServiceDesc.ServiceName),
		func(query url.Values, attr infra.NodeMeta, addr *resolver.Address) bool {
			return true
		})

	a.centerAsyncFinder = finder
}

func (a *app) getCenterConnect(ctx context.Context, region, addr string) (*conncache.GRPCConn[CenterConnCacheKey, *grpc.ClientConn], error) {
	cc, err := a.__centerConncets.GetConn(ctx, CenterConnCacheKey{
		Endpoint: addr,
		Region:   region,
	})
	if err != nil {
		return nil, err
	}
	return cc, nil
}

func (a *app) GetCenterSrvList() ([]*CenterClient, error) {
	addrs := a.centerAsyncFinder.GetCurrentResults()

	ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
	defer cancel()

	var list []*CenterClient
	for _, addr := range addrs {
		attr, ok := infra.GetNodeMetaAttribute(addr)
		if !ok {
			wlog.Error("failed to get resolve address balance attributes", zap.String("addr", addr.Addr))
			return nil, fmt.Errorf("failed to get balance attribute, address: %s", addr.Addr)
		}

		cc, err := a.getCenterConnect(ctx, attr.Region, addr.Addr)
		if err != nil {
			return nil, err
		}
		list = append(list, &CenterClient{
			CenterClient: cronpb.NewCenterClient(cc.ClientConn()),
			addr:         addr.Addr,
			cancel: func() {
				cc.Done()
			},
		})
	}
	return list, nil
}

func (a *app) DispatchEvent(event *cronpb.SendEventRequest) error {
	if event.Event.Type == cronpb.EventType_EVENT_UNKNOWN {
		return fmt.Errorf("event type is undefined")
	}
	mtimer := a.metrics.CustomHistogramSet("dispatch_event")
	defer mtimer.ObserveDuration()
	centers, err := a.GetCenterSrvList()
	if err != nil {
		return err
	}

	dispatchOne := func(v *CenterClient) error {
		defer v.Close()
		ctx, cancel := context.WithTimeout(a.ctx, time.Duration(a.GetConfig().Deploy.Timeout)*time.Second)
		defer cancel()
		if _, err := v.SendEvent(ctx, event); err != nil {
			a.metrics.CustomInc("send_event_error", v.addr, err.Error())
			return fmt.Errorf("failed to send event to %s, error: %s", v.addr, err.Error())
		}
		return nil
	}

	for _, v := range centers {
		if err := dispatchOne(v); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) GetGrpcDirector() func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
	return func(ctx context.Context, fullMethodName string) (context.Context, *grpc.ClientConn, error) {
		var (
			cc  *grpc.ClientConn
			err error
		)
		go safe.Run(func() {
			// 根据上下文关闭链接
			for {
				select {
				case <-ctx.Done():
					if cc != nil {
						cc.Close()
					}
					return
				}
			}
		})
		md, _ := metadata.FromIncomingContext(ctx)
		wlog.Debug("got proxy request", zap.String("full_method", fullMethodName))
		addrs := md.Get(common.GOPHERCRON_PROXY_TO_MD_KEY)
		dialOptions := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		if len(addrs) > 0 {
			addr := addrs[0]
			wlog.Debug("address proxy", zap.String("proxy_to", addr), zap.String("full_method", fullMethodName))
			if cc, err = grpc.DialContext(ctx, addr, dialOptions...); err != nil {
				return nil, nil, err
			}

			md.Set(common.GOPHERCRON_AGENT_IP_MD_KEY, "gophercron_proxy")
			outCtx := metadata.NewOutgoingContext(ctx, md.Copy())
			return outCtx, cc, nil
		} else {
			projectIDs := md.Get(common.GOPHERCRON_PROXY_PROJECT_MD_KEY)
			wlog.Debug("resolve proxy", zap.Any("project", projectIDs), zap.String("full_method", fullMethodName))
			if len(projectIDs) == 0 {
				return nil, nil, status.Error(codes.Unknown, "undefined project id")
			}
			projectID, err := strconv.ParseInt(projectIDs[0], 10, 64)
			if err != nil {
				return nil, nil, status.Error(codes.Unknown, "invalid project id")
			}
			ls := strings.Split(fullMethodName, "/")
			if len(ls) != 3 {
				return nil, nil, status.Error(codes.Unknown, "unknown full method name")
			}
			newCC := infra.NewClientConn()
			cc, err := newCC(ls[1], newCC.WithSystem(projectID), newCC.WithOrg(a.cfg.Micro.OrgID), newCC.WithRegion(a.cfg.Micro.Region),
				newCC.WithServiceResolver(etcd.NewEtcdResolver(infra.ResolveEtcdClient(), infra.ProxyAllowFunc)),
				newCC.WithGrpcDialOptions(dialOptions...))
			if err != nil {
				return nil, nil, err
			}
			outCtx := metadata.NewOutgoingContext(ctx, md.Copy())
			return outCtx, cc, nil
		}
	}
}

func BuildProxyDialerInfo(ctx context.Context, region, address string, opts []grpc.DialOption) (dialAddress string, gopts []grpc.DialOption) {
	dialAddress = infra.ResolveProxy(region)
	if dialAddress == "" {
		wlog.Error("proxy address not found", zap.String("region", region))
	}
	genMetadata := func(ctx context.Context) context.Context {
		md, exist := metadata.FromOutgoingContext(ctx)
		if !exist {
			md = metadata.New(map[string]string{})
		}
		md.Set(common.GOPHERCRON_PROXY_TO_MD_KEY, address)
		return metadata.NewOutgoingContext(ctx, md)
	}
	gopts = append(opts, grpc.WithUnaryInterceptor(func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(genMetadata(ctx), method, req, reply, cc, opts...)
	}),
		grpc.WithStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(genMetadata(ctx), desc, cc, method, opts...)
		}))
	return dialAddress, gopts
}

type Authenticator struct {
	privateKey []byte
	token      string
	expireTime time.Time
}

func (s *Authenticator) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	if s.expireTime.After(time.Now().Add(time.Second * 10)) {
		return map[string]string{
			common.GOPHERCRON_CENTER_AUTH_KEY: s.token,
		}, nil
	}
	claims := jwt.CenterTokenClaims{
		Biz: jwt.DefaultBIZ,
		Iat: time.Now().Unix(),
		Exp: time.Now().Add(time.Hour).Unix(),
	}
	token, err := jwt.BuildCenterJWT(claims, s.privateKey)
	if err != nil {
		return nil, err
	}

	s.token = token
	s.expireTime = time.Unix(claims.Exp, 0)

	return map[string]string{
		common.GOPHERCRON_CENTER_AUTH_KEY: token,
	}, nil
}

func (s *Authenticator) RequireTransportSecurity() bool {
	return false
}
