// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/allocator"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/internal/util/typeutil"
)

type UniqueID = typeutil.UniqueID
type Timestamp = typeutil.Timestamp

const sendTimeTickMsgInterval = 200 * time.Millisecond
const channelMgrTickerInterval = 100 * time.Millisecond

type Proxy struct {
	ctx    context.Context
	cancel func()
	wg     sync.WaitGroup

	initParams *internalpb.InitParams
	ip         string
	port       int

	stateCode atomic.Value

	rootCoord  types.RootCoord
	indexCoord types.IndexCoord
	dataCoord  types.DataCoord
	queryCoord types.QueryCoord

	chMgr channelsMgr

	sched *TaskScheduler
	tick  *timeTick

	chTicker channelsTimeTicker

	idAllocator  *allocator.IDAllocator
	tsoAllocator *TimestampAllocator
	segAssigner  *SegIDAssigner

	session *sessionutil.Session

	msFactory msgstream.Factory

	// Add callback functions at different stages
	startCallbacks []func()
	closeCallbacks []func()
}

func NewProxy(ctx context.Context, factory msgstream.Factory) (*Proxy, error) {
	rand.Seed(time.Now().UnixNano())
	ctx1, cancel := context.WithCancel(ctx)
	node := &Proxy{
		ctx:       ctx1,
		cancel:    cancel,
		msFactory: factory,
	}
	node.UpdateStateCode(internalpb.StateCode_Abnormal)
	log.Debug("Proxy", zap.Any("State", node.stateCode.Load()))
	return node, nil

}

// Register register proxy at etcd
func (node *Proxy) Register() error {
	node.session = sessionutil.NewSession(node.ctx, Params.MetaRootPath, Params.EtcdEndpoints)
	node.session.Init(typeutil.ProxyRole, Params.NetworkAddress, false)
	Params.ProxyID = node.session.ServerID
	Params.initProxySubName()
	return nil
}

func (node *Proxy) Init() error {
	// wait for datacoord state changed to Healthy
	if node.dataCoord != nil {
		log.Debug("Proxy wait for dataCoord ready")
		err := funcutil.WaitForComponentHealthy(node.ctx, node.dataCoord, "DataCoord", 1000000, time.Millisecond*200)
		if err != nil {
			log.Debug("Proxy wait for dataCoord ready failed", zap.Error(err))
			return err
		}
		log.Debug("Proxy dataCoord is ready")
	}

	// wait for queryCoord state changed to Healthy
	if node.queryCoord != nil {
		log.Debug("Proxy wait for queryCoord ready")
		err := funcutil.WaitForComponentHealthy(node.ctx, node.queryCoord, "QueryCoord", 1000000, time.Millisecond*200)
		if err != nil {
			log.Debug("Proxy wait for queryCoord ready failed", zap.Error(err))
			return err
		}
		log.Debug("Proxy queryCoord is ready")
	}

	// wait for indexcoord state changed to Healthy
	if node.indexCoord != nil {
		log.Debug("Proxy wait for indexCoord ready")
		err := funcutil.WaitForComponentHealthy(node.ctx, node.indexCoord, "IndexCoord", 1000000, time.Millisecond*200)
		if err != nil {
			log.Debug("Proxy wait for indexCoord ready failed", zap.Error(err))
			return err
		}
		log.Debug("Proxy indexCoord is ready")
	}

	if node.queryCoord != nil {
		resp, err := node.queryCoord.CreateQueryChannel(node.ctx, &querypb.CreateQueryChannelRequest{})
		if err != nil {
			log.Debug("Proxy CreateQueryChannel failed", zap.Error(err))
			return err
		}
		if resp.Status.ErrorCode != commonpb.ErrorCode_Success {
			log.Debug("Proxy CreateQueryChannel failed", zap.String("reason", resp.Status.Reason))

			return errors.New(resp.Status.Reason)
		}
		log.Debug("Proxy CreateQueryChannel success")

		Params.SearchResultChannelNames = []string{resp.ResultChannel}
		Params.RetrieveResultChannelNames = []string{resp.ResultChannel}
		log.Debug("Proxy CreateQueryChannel success", zap.Any("SearchResultChannelNames", Params.SearchResultChannelNames))
		log.Debug("Proxy CreateQueryChannel success", zap.Any("RetrieveResultChannelNames", Params.RetrieveResultChannelNames))
	}

	m := map[string]interface{}{
		"PulsarAddress": Params.PulsarAddress,
		"PulsarBufSize": 1024}
	err := node.msFactory.SetParams(m)
	if err != nil {
		return err
	}

	idAllocator, err := allocator.NewIDAllocator(node.ctx, Params.MetaRootPath, Params.EtcdEndpoints)

	if err != nil {
		return err
	}
	node.idAllocator = idAllocator
	node.idAllocator.PeerID = Params.ProxyID

	tsoAllocator, err := NewTimestampAllocator(node.ctx, node.rootCoord, Params.ProxyID)
	if err != nil {
		return err
	}
	node.tsoAllocator = tsoAllocator

	segAssigner, err := NewSegIDAssigner(node.ctx, node.dataCoord, node.lastTick)
	if err != nil {
		panic(err)
	}
	node.segAssigner = segAssigner
	node.segAssigner.PeerID = Params.ProxyID

	getDmlChannelsFunc := func(collectionID UniqueID) (map[vChan]pChan, error) {
		req := &milvuspb.DescribeCollectionRequest{
			Base: &commonpb.MsgBase{
				MsgType:   commonpb.MsgType_DescribeCollection,
				MsgID:     0, // todo
				Timestamp: 0, // todo
				SourceID:  0, // todo
			},
			DbName:         "", // todo
			CollectionName: "", // todo
			CollectionID:   collectionID,
			TimeStamp:      0, // todo
		}
		resp, err := node.rootCoord.DescribeCollection(node.ctx, req)
		if err != nil {
			log.Warn("DescribeCollection", zap.Error(err))
			return nil, err
		}
		if resp.Status.ErrorCode != 0 {
			log.Warn("DescribeCollection",
				zap.Any("ErrorCode", resp.Status.ErrorCode),
				zap.Any("Reason", resp.Status.Reason))
			return nil, err
		}
		if len(resp.VirtualChannelNames) != len(resp.PhysicalChannelNames) {
			err := fmt.Errorf(
				"len(VirtualChannelNames): %v, len(PhysicalChannelNames): %v",
				len(resp.VirtualChannelNames),
				len(resp.PhysicalChannelNames))
			log.Warn("GetDmlChannels", zap.Error(err))
			return nil, err
		}

		ret := make(map[vChan]pChan)
		for idx, name := range resp.VirtualChannelNames {
			if _, ok := ret[name]; ok {
				err := fmt.Errorf(
					"duplicated virtual channel found, vchan: %v, pchan: %v",
					name,
					resp.PhysicalChannelNames[idx])
				return nil, err
			}
			ret[name] = resp.PhysicalChannelNames[idx]
		}

		return ret, nil
	}
	getDqlChannelsFunc := func(collectionID UniqueID) (map[vChan]pChan, error) {
		req := &querypb.CreateQueryChannelRequest{
			CollectionID: collectionID,
			ProxyID:      node.session.ServerID,
		}
		resp, err := node.queryCoord.CreateQueryChannel(node.ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Status.ErrorCode != commonpb.ErrorCode_Success {
			return nil, errors.New(resp.Status.Reason)
		}

		m := make(map[vChan]pChan)
		m[resp.RequestChannel] = resp.RequestChannel

		return m, nil
	}

	chMgr := newChannelsMgr(getDmlChannelsFunc, defaultInsertRepackFunc, getDqlChannelsFunc, nil, node.msFactory)
	node.chMgr = chMgr

	node.sched, err = NewTaskScheduler(node.ctx, node.idAllocator, node.tsoAllocator, node.msFactory)
	if err != nil {
		return err
	}

	node.tick = newTimeTick(node.ctx, node.tsoAllocator, time.Millisecond*200, node.sched.TaskDoneTest, node.msFactory)

	node.chTicker = newChannelsTimeTicker(node.ctx, channelMgrTickerInterval, []string{}, node.sched.getPChanStatistics, tsoAllocator)

	return nil
}

func (node *Proxy) sendChannelsTimeTickLoop() {
	node.wg.Add(1)
	go func() {
		defer node.wg.Done()

		// TODO(dragondriver): read this from config
		timer := time.NewTicker(sendTimeTickMsgInterval)

		for {
			select {
			case <-node.ctx.Done():
				return
			case <-timer.C:
				ts, err := node.tsoAllocator.AllocOne()
				if err != nil {
					log.Warn("Failed to get timestamp from tso", zap.Error(err))
					continue
				}

				stats, err := node.chTicker.getMinTsStatistics()
				if err != nil {
					log.Warn("sendChannelsTimeTickLoop.getMinTsStatistics", zap.Error(err))
					continue
				}

				channels := make([]pChan, 0, len(stats))
				tss := make([]Timestamp, 0, len(stats))

				maxTs := ts
				for channel, ts := range stats {
					channels = append(channels, channel)
					tss = append(tss, ts)
					if ts > maxTs {
						maxTs = ts
					}
				}

				log.Debug("send timestamp statistics of pchan", zap.Any("channels", channels), zap.Any("tss", tss))

				req := &internalpb.ChannelTimeTickMsg{
					Base: &commonpb.MsgBase{
						MsgType:   commonpb.MsgType_TimeTick, // todo
						MsgID:     0,                         // todo
						Timestamp: 0,                         // todo
						SourceID:  node.session.ServerID,
					},
					ChannelNames:     channels,
					Timestamps:       tss,
					DefaultTimestamp: maxTs,
				}

				status, err := node.rootCoord.UpdateChannelTimeTick(node.ctx, req)
				if err != nil {
					log.Warn("sendChannelsTimeTickLoop.UpdateChannelTimeTick", zap.Error(err))
					continue
				}
				if status.ErrorCode != 0 {
					log.Warn("sendChannelsTimeTickLoop.UpdateChannelTimeTick",
						zap.Any("ErrorCode", status.ErrorCode),
						zap.Any("Reason", status.Reason))
					continue
				}
			}
		}
	}()
}

func (node *Proxy) Start() error {
	err := InitMetaCache(node.rootCoord)
	if err != nil {
		return err
	}
	log.Debug("init global meta cache ...")

	node.sched.Start()
	log.Debug("start scheduler ...")

	node.idAllocator.Start()
	log.Debug("start id allocator ...")

	node.segAssigner.Start()
	log.Debug("start seg assigner ...")

	node.tick.Start()
	log.Debug("start time tick ...")

	err = node.chTicker.start()
	if err != nil {
		return err
	}
	log.Debug("start channelsTimeTicker")

	node.sendChannelsTimeTickLoop()

	// Start callbacks
	for _, cb := range node.startCallbacks {
		cb()
	}

	node.UpdateStateCode(internalpb.StateCode_Healthy)
	log.Debug("Proxy", zap.Any("State", node.stateCode.Load()))

	return nil
}

func (node *Proxy) Stop() error {
	node.cancel()

	if node.idAllocator != nil {
		node.idAllocator.Close()
	}
	if node.segAssigner != nil {
		node.segAssigner.Close()
	}
	if node.sched != nil {
		node.sched.Close()
	}
	if node.tick != nil {
		node.tick.Close()
	}
	if node.chTicker != nil {
		err := node.chTicker.close()
		if err != nil {
			return err
		}
	}

	node.wg.Wait()

	for _, cb := range node.closeCallbacks {
		cb()
	}

	return nil
}

// AddStartCallback adds a callback in the startServer phase.
func (node *Proxy) AddStartCallback(callbacks ...func()) {
	node.startCallbacks = append(node.startCallbacks, callbacks...)
}

func (node *Proxy) lastTick() Timestamp {
	return node.tick.LastTick()
}

// AddCloseCallback adds a callback in the Close phase.
func (node *Proxy) AddCloseCallback(callbacks ...func()) {
	node.closeCallbacks = append(node.closeCallbacks, callbacks...)
}

func (node *Proxy) SetRootCoordClient(cli types.RootCoord) {
	node.rootCoord = cli
}

func (node *Proxy) SetIndexCoordClient(cli types.IndexCoord) {
	node.indexCoord = cli
}

func (node *Proxy) SetDataCoordClient(cli types.DataCoord) {
	node.dataCoord = cli
}

func (node *Proxy) SetQueryCoordClient(cli types.QueryCoord) {
	node.queryCoord = cli
}
