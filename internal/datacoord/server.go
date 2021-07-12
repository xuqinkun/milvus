// Copyright (C) 2019-2020 Zilliz. All rights reserved.//// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package datacoord

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	datanodeclient "github.com/milvus-io/milvus/internal/distributed/datanode/client"
	rootcoordclient "github.com/milvus-io/milvus/internal/distributed/rootcoord/client"
	"github.com/milvus-io/milvus/internal/logutil"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"

	etcdkv "github.com/milvus-io/milvus/internal/kv/etcd"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/retry"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"

	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
)

const (
	rootCoordClientTimout = 20 * time.Second
	connEtcdMaxRetryTime  = 100000
	connEtcdRetryInterval = 200 * time.Millisecond
)

type (
	UniqueID  = typeutil.UniqueID
	Timestamp = typeutil.Timestamp
)

// ServerState type alias
type ServerState = int64

const (
	// ServerStateStopped state stands for just created or stopped `Server` instance
	ServerStateStopped ServerState = 0
	// ServerStateInitializing state stands initializing `Server` instance
	ServerStateInitializing ServerState = 1
	// ServerStateHealthy state stands for healthy `Server` instance
	ServerStateHealthy ServerState = 2
)

type dataNodeCreatorFunc func(ctx context.Context, addr string) (types.DataNode, error)
type rootCoordCreatorFunc func(ctx context.Context, metaRootPath string, etcdEndpoints []string) (types.RootCoord, error)

// Server implements `types.Datacoord`
// handles Data Cooridinator related jobs
type Server struct {
	ctx              context.Context
	serverLoopCtx    context.Context
	serverLoopCancel context.CancelFunc
	serverLoopWg     sync.WaitGroup
	isServing        ServerState

	kvClient        *etcdkv.EtcdKV
	meta            *meta
	segmentManager  Manager
	allocator       allocator
	cluster         *Cluster
	rootCoordClient types.RootCoord
	ddChannelName   string

	flushCh   chan UniqueID
	msFactory msgstream.Factory

	session  *sessionutil.Session
	activeCh <-chan bool
	eventCh  <-chan *sessionutil.SessionEvent

	dataClientCreator      dataNodeCreatorFunc
	rootCoordClientCreator rootCoordCreatorFunc
}

// CreateServer create `Server` instance
func CreateServer(ctx context.Context, factory msgstream.Factory) (*Server, error) {
	rand.Seed(time.Now().UnixNano())
	s := &Server{
		ctx:                    ctx,
		msFactory:              factory,
		flushCh:                make(chan UniqueID, 1024),
		dataClientCreator:      defaultDataNodeCreatorFunc,
		rootCoordClientCreator: defaultRootCoordCreatorFunc,
	}
	return s, nil
}

func defaultDataNodeCreatorFunc(ctx context.Context, addr string) (types.DataNode, error) {
	return datanodeclient.NewClient(ctx, addr)
}

func defaultRootCoordCreatorFunc(ctx context.Context, metaRootPath string, etcdEndpoints []string) (types.RootCoord, error) {
	return rootcoordclient.NewClient(ctx, metaRootPath, etcdEndpoints)
}

// Register register data service at etcd
func (s *Server) Register() error {
	s.session = sessionutil.NewSession(s.ctx, Params.MetaRootPath, Params.EtcdEndpoints)
	s.activeCh = s.session.Init(typeutil.DataCoordRole, Params.IP, true)
	Params.NodeID = s.session.ServerID
	return nil
}

// Init change server state to Initializing
func (s *Server) Init() error {
	atomic.StoreInt64(&s.isServing, ServerStateInitializing)
	return nil
}

// Start initialize `Server` members and start loops, follow steps are taken:
// 1. initialize message factory parameters
// 2. initialize root coord client, meta, datanode cluster, segment info channel,
//		allocator, segment manager
// 3. start service discovery and server loops, which includes message stream handler (segment statistics,datanode tt)
//		datanodes etcd watch, etcd alive check and flush completed status check
// 4. set server state to Healthy
func (s *Server) Start() error {
	var err error
	m := map[string]interface{}{
		"PulsarAddress":  Params.PulsarAddress,
		"ReceiveBufSize": 1024,
		"PulsarBufSize":  1024}
	err = s.msFactory.SetParams(m)
	if err != nil {
		return err
	}
	if err = s.initRootCoordClient(); err != nil {
		return err
	}

	if err = s.initMeta(); err != nil {
		return err
	}

	if err = s.initCluster(); err != nil {
		return err
	}

	s.allocator = newRootCoordAllocator(s.ctx, s.rootCoordClient)

	s.startSegmentManager()
	if err = s.initServiceDiscovery(); err != nil {
		return err
	}

	s.startServerLoop()

	atomic.StoreInt64(&s.isServing, ServerStateHealthy)
	log.Debug("DataCoordinator startup success")
	return nil
}

func (s *Server) initCluster() error {
	var err error
	s.cluster, err = NewCluster(s.ctx, s.kvClient, NewNodesInfo(), s)
	return err
}

func (s *Server) initServiceDiscovery() error {
	sessions, rev, err := s.session.GetSessions(typeutil.DataNodeRole)
	if err != nil {
		log.Debug("DataCoord initMeta failed", zap.Error(err))
		return err
	}
	log.Debug("registered sessions", zap.Any("sessions", sessions))

	datanodes := make([]*NodeInfo, 0, len(sessions))
	for _, session := range sessions {
		info := &datapb.DataNodeInfo{
			Address:  session.Address,
			Version:  session.ServerID,
			Channels: []*datapb.ChannelStatus{},
		}
		nodeInfo := NewNodeInfo(s.ctx, info)
		datanodes = append(datanodes, nodeInfo)
	}

	s.cluster.Startup(datanodes)

	s.eventCh = s.session.WatchServices(typeutil.DataNodeRole, rev+1)
	return nil
}

func (s *Server) loadDataNodes() []*datapb.DataNodeInfo {
	if s.session == nil {
		log.Warn("load data nodes but session is nil")
		return []*datapb.DataNodeInfo{}
	}
	sessions, _, err := s.session.GetSessions(typeutil.DataNodeRole)
	if err != nil {
		log.Warn("load data nodes faild", zap.Error(err))
		return []*datapb.DataNodeInfo{}
	}
	datanodes := make([]*datapb.DataNodeInfo, 0, len(sessions))
	for _, session := range sessions {
		datanodes = append(datanodes, &datapb.DataNodeInfo{
			Address:  session.Address,
			Version:  session.ServerID,
			Channels: []*datapb.ChannelStatus{},
		})
	}
	return datanodes
}

func (s *Server) startSegmentManager() {
	s.segmentManager = newSegmentManager(s.meta, s.allocator)
}

func (s *Server) initMeta() error {
	connectEtcdFn := func() error {
		etcdClient, err := clientv3.New(clientv3.Config{Endpoints: Params.EtcdEndpoints})
		if err != nil {
			return err
		}
		s.kvClient = etcdkv.NewEtcdKV(etcdClient, Params.MetaRootPath)
		s.meta, err = newMeta(s.kvClient)
		if err != nil {
			return err
		}
		return nil
	}
	return retry.Do(s.ctx, connectEtcdFn, retry.Attempts(connEtcdMaxRetryTime))
}

func (s *Server) startServerLoop() {
	s.serverLoopCtx, s.serverLoopCancel = context.WithCancel(s.ctx)
	s.serverLoopWg.Add(5)
	go s.startStatsChannel(s.serverLoopCtx)
	go s.startDataNodeTtLoop(s.serverLoopCtx)
	go s.startWatchService(s.serverLoopCtx)
	go s.startActiveCheck(s.serverLoopCtx)
	go s.startFlushLoop(s.serverLoopCtx)
}

func (s *Server) startStatsChannel(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	statsStream, _ := s.msFactory.NewMsgStream(ctx)
	statsStream.AsConsumer([]string{Params.StatisticsChannelName}, Params.DataCoordSubscriptionName)
	log.Debug("DataCoord stats stream",
		zap.String("channelName", Params.StatisticsChannelName),
		zap.String("descriptionName", Params.DataCoordSubscriptionName))
	statsStream.Start()
	defer statsStream.Close()
	for {
		select {
		case <-ctx.Done():
			log.Debug("stats channel shutdown")
			return
		default:
		}
		msgPack := statsStream.Consume()
		if msgPack == nil {
			return
		}
		for _, msg := range msgPack.Msgs {
			if msg.Type() != commonpb.MsgType_SegmentStatistics {
				log.Warn("receive unknown msg from segment statistics channel",
					zap.Stringer("msgType", msg.Type()))
				continue
			}
			log.Debug("Receive DataNode segment statistics update")
			ssMsg := msg.(*msgstream.SegmentStatisticsMsg)
			for _, stat := range ssMsg.SegStats {
				s.segmentManager.UpdateSegmentStats(stat)
			}
		}
	}
}

func (s *Server) startDataNodeTtLoop(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	ttMsgStream, err := s.msFactory.NewMsgStream(ctx)
	if err != nil {
		log.Error("new msg stream failed", zap.Error(err))
		return
	}
	ttMsgStream.AsConsumer([]string{Params.TimeTickChannelName},
		Params.DataCoordSubscriptionName)
	log.Debug(fmt.Sprintf("DataCoord AsConsumer:%s:%s",
		Params.TimeTickChannelName, Params.DataCoordSubscriptionName))
	ttMsgStream.Start()
	defer ttMsgStream.Close()
	for {
		select {
		case <-ctx.Done():
			log.Debug("data node tt loop shutdown")
			return
		default:
		}
		msgPack := ttMsgStream.Consume()
		if msgPack == nil {
			return
		}
		for _, msg := range msgPack.Msgs {
			if msg.Type() != commonpb.MsgType_DataNodeTt {
				log.Warn("Receive unexpected msg type from tt channel",
					zap.Stringer("msgType", msg.Type()))
				continue
			}
			ttMsg := msg.(*msgstream.DataNodeTtMsg)

			ch := ttMsg.ChannelName
			ts := ttMsg.Timestamp
			// log.Debug("Receive datanode timetick msg", zap.String("channel", ch),
			// zap.Any("ts", ts))
			segments, err := s.segmentManager.GetFlushableSegments(ctx, ch, ts)
			if err != nil {
				log.Warn("get flushable segments failed", zap.Error(err))
				continue
			}

			if len(segments) == 0 {
				continue
			}
			log.Debug("Flush segments", zap.Int64s("segmentIDs", segments))
			segmentInfos := make([]*datapb.SegmentInfo, 0, len(segments))
			for _, id := range segments {
				sInfo := s.meta.GetSegment(id)
				if sInfo == nil {
					log.Error("get segment from meta error", zap.Int64("id", id),
						zap.Error(err))
					continue
				}
				segmentInfos = append(segmentInfos, sInfo)
			}
			if len(segmentInfos) > 0 {
				s.cluster.Flush(segmentInfos)
			}
			s.segmentManager.ExpireAllocations(ch, ts)
		}
	}
}

func (s *Server) startWatchService(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	for {
		select {
		case <-ctx.Done():
			log.Debug("watch service shutdown")
			return
		case event := <-s.eventCh:
			info := &datapb.DataNodeInfo{
				Address:  event.Session.Address,
				Version:  event.Session.ServerID,
				Channels: []*datapb.ChannelStatus{},
			}
			node := NewNodeInfo(ctx, info)
			switch event.EventType {
			case sessionutil.SessionAddEvent:
				log.Info("Received datanode register",
					zap.String("address", info.Address),
					zap.Int64("serverID", info.Version))
				s.cluster.Register(node)
			case sessionutil.SessionDelEvent:
				log.Info("Received datanode unregister",
					zap.String("address", info.Address),
					zap.Int64("serverID", info.Version))
				s.cluster.UnRegister(node)
			default:
				log.Warn("receive unknown service event type",
					zap.Any("type", event.EventType))
			}
		}
	}
}

func (s *Server) startActiveCheck(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()

	for {
		select {
		case _, ok := <-s.activeCh:
			if ok {
				continue
			}
			s.Stop()
			log.Debug("disconnect with etcd and shutdown data coordinator")
			return
		case <-ctx.Done():
			log.Debug("connection check shutdown")
			return
		}
	}
}

func (s *Server) startFlushLoop(ctx context.Context) {
	defer logutil.LogPanic()
	defer s.serverLoopWg.Done()
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	// send `Flushing` segments
	go s.handleFlushingSegments(ctx2)
	for {
		select {
		case <-ctx.Done():
			log.Debug("flush loop shutdown")
			return
		case segmentID := <-s.flushCh:
			segment := s.meta.GetSegment(segmentID)
			if segment == nil {
				log.Warn("failed to get flused segment", zap.Int64("id", segmentID))
				continue
			}
			req := &datapb.SegmentFlushCompletedMsg{
				Base: &commonpb.MsgBase{
					MsgType: commonpb.MsgType_SegmentFlushDone,
				},
				Segment: segment,
			}
			resp, err := s.rootCoordClient.SegmentFlushCompleted(ctx, req)
			if err = VerifyResponse(resp, err); err != nil {
				log.Warn("failed to call SegmentFlushComplete", zap.Int64("segmentID", segmentID), zap.Error(err))
				continue
			}
			// set segment to SegmentState_Flushed
			if err = s.meta.SetState(segmentID, commonpb.SegmentState_Flushed); err != nil {
				log.Error("flush segment complete failed", zap.Error(err))
				continue
			}
			log.Debug("flush segment complete", zap.Int64("id", segmentID))
		}
	}
}

func (s *Server) handleFlushingSegments(ctx context.Context) {
	segments := s.meta.GetFlushingSegments()
	for _, segment := range segments {
		select {
		case <-ctx.Done():
			return
		case s.flushCh <- segment.ID:
		}
	}
}

func (s *Server) initRootCoordClient() error {
	var err error
	if s.rootCoordClient, err = s.rootCoordClientCreator(s.ctx, Params.MetaRootPath, Params.EtcdEndpoints); err != nil {
		return err
	}
	if err = s.rootCoordClient.Init(); err != nil {
		return err
	}
	return s.rootCoordClient.Start()
}

// Stop do the Server finalize processes
// it checks the server status is healthy, if not, just quit
// if Server is healthy, set server state to stopped, release etcd session,
//	stop message stream client and stop server loops
func (s *Server) Stop() error {
	if !atomic.CompareAndSwapInt64(&s.isServing, ServerStateHealthy, ServerStateStopped) {
		return nil
	}
	log.Debug("DataCoord server shutdown")
	atomic.StoreInt64(&s.isServing, ServerStateStopped)
	s.cluster.Close()
	s.stopServerLoop()
	return nil
}

// CleanMeta only for test
func (s *Server) CleanMeta() error {
	log.Debug("clean meta", zap.Any("kv", s.kvClient))
	return s.kvClient.RemoveWithPrefix("")
}

func (s *Server) stopServerLoop() {
	s.serverLoopCancel()
	s.serverLoopWg.Wait()
}

//func (s *Server) validateAllocRequest(collID UniqueID, partID UniqueID, channelName string) error {
//	if !s.meta.HasCollection(collID) {
//		return fmt.Errorf("can not find collection %d", collID)
//	}
//	if !s.meta.HasPartition(collID, partID) {
//		return fmt.Errorf("can not find partition %d", partID)
//	}
//	for _, name := range s.insertChannels {
//		if name == channelName {
//			return nil
//		}
//	}
//	return fmt.Errorf("can not find channel %s", channelName)
//}

func (s *Server) loadCollectionFromRootCoord(ctx context.Context, collectionID int64) error {
	resp, err := s.rootCoordClient.DescribeCollection(ctx, &milvuspb.DescribeCollectionRequest{
		Base: &commonpb.MsgBase{
			MsgType:  commonpb.MsgType_DescribeCollection,
			SourceID: Params.NodeID,
		},
		DbName:       "",
		CollectionID: collectionID,
	})
	if err = VerifyResponse(resp, err); err != nil {
		return err
	}
	presp, err := s.rootCoordClient.ShowPartitions(ctx, &milvuspb.ShowPartitionsRequest{
		Base: &commonpb.MsgBase{
			MsgType:   commonpb.MsgType_ShowPartitions,
			MsgID:     0,
			Timestamp: 0,
			SourceID:  Params.NodeID,
		},
		DbName:         "",
		CollectionName: resp.Schema.Name,
		CollectionID:   resp.CollectionID,
	})
	if err = VerifyResponse(presp, err); err != nil {
		log.Error("show partitions error", zap.String("collectionName", resp.Schema.Name),
			zap.Int64("collectionID", resp.CollectionID), zap.Error(err))
		return err
	}
	collInfo := &datapb.CollectionInfo{
		ID:         resp.CollectionID,
		Schema:     resp.Schema,
		Partitions: presp.PartitionIDs,
	}
	s.meta.AddCollection(collInfo)
	return nil
}

func (s *Server) prepareBinlog(req *datapb.SaveBinlogPathsRequest) (map[string]string, error) {
	meta := make(map[string]string)

	for _, fieldBlp := range req.Field2BinlogPaths {
		fieldMeta, err := s.prepareField2PathMeta(req.SegmentID, fieldBlp)
		if err != nil {
			return nil, err
		}
		for k, v := range fieldMeta {
			meta[k] = v
		}
	}

	return meta, nil
}
