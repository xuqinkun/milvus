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
package datacoord

import (
	"fmt"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/milvus-io/milvus/internal/kv"

	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
)

const (
	metaPrefix    = "datacoord-meta"
	segmentPrefix = metaPrefix + "/s"
)

type meta struct {
	sync.RWMutex
	client      kv.TxnKV                            // client of a reliable kv service, i.e. etcd client
	collections map[UniqueID]*datapb.CollectionInfo // collection id to collection info
	segments    *SegmentsInfo                       // segment id to segment info
}

func newMeta(kv kv.TxnKV) (*meta, error) {
	mt := &meta{
		client:      kv,
		collections: make(map[UniqueID]*datapb.CollectionInfo),
		segments:    NewSegmentsInfo(),
	}
	err := mt.reloadFromKV()
	if err != nil {
		return nil, err
	}
	return mt, nil
}

func (m *meta) reloadFromKV() error {
	_, values, err := m.client.LoadWithPrefix(segmentPrefix)
	if err != nil {
		return err
	}

	for _, value := range values {
		segmentInfo := &datapb.SegmentInfo{}
		err = proto.UnmarshalText(value, segmentInfo)
		if err != nil {
			return fmt.Errorf("DataCoord reloadFromKV UnMarshalText datapb.SegmentInfo err:%w", err)
		}
		m.segments.SetSegment(segmentInfo.GetID(), segmentInfo)
	}

	return nil
}

func (m *meta) AddCollection(collection *datapb.CollectionInfo) {
	m.Lock()
	defer m.Unlock()
	m.collections[collection.ID] = collection
}

func (m *meta) GetCollection(collectionID UniqueID) *datapb.CollectionInfo {
	m.RLock()
	defer m.RUnlock()
	collection, ok := m.collections[collectionID]
	if !ok {
		return nil
	}
	return collection
}

func (m *meta) GetNumRowsOfCollection(collectionID UniqueID) int64 {
	m.RLock()
	defer m.RUnlock()
	var ret int64 = 0
	segments := m.segments.GetSegments()
	for _, segment := range segments {
		if segment.GetCollectionID() == collectionID {
			ret += segment.GetNumOfRows()
		}
	}
	return ret
}

func (m *meta) AddSegment(segment *datapb.SegmentInfo) error {
	m.Lock()
	defer m.Unlock()
	m.segments.SetSegment(segment.GetID(), segment)
	if err := m.saveSegmentInfo(segment); err != nil {
		return err
	}
	return nil
}

func (m *meta) SetRowCount(segmentID UniqueID, rowCount int64) error {
	m.Lock()
	defer m.Unlock()
	m.segments.SetRowCount(segmentID, rowCount)
	if segment := m.segments.GetSegment(segmentID); segment != nil {
		return m.saveSegmentInfo(segment)
	}
	return nil
}

func (m *meta) SetLastExpireTime(segmentID UniqueID, expireTs Timestamp) error {
	m.Lock()
	defer m.Unlock()
	m.segments.SetLasteExpiraTime(segmentID, expireTs)
	if segment := m.segments.GetSegment(segmentID); segment != nil {
		return m.saveSegmentInfo(segment)
	}
	return nil
}

func (m *meta) DropSegment(segmentID UniqueID) error {
	m.Lock()
	defer m.Unlock()
	segment := m.segments.GetSegment(segmentID)
	m.segments.DropSegment(segmentID)
	if err := m.removeSegmentInfo(segment); err != nil {
		return err
	}
	return nil
}

func (m *meta) GetSegment(segID UniqueID) *datapb.SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	return m.segments.GetSegment(segID)
}

func (m *meta) SetState(segmentID UniqueID, state commonpb.SegmentState) error {
	m.Lock()
	defer m.Unlock()
	m.segments.SetState(segmentID, state)
	if segInfo := m.segments.GetSegment(segmentID); segInfo != nil {
		return m.saveSegmentInfo(segInfo)
	}
	return nil
}

func (m *meta) SaveBinlogAndCheckPoints(segID UniqueID, flushed bool,
	binlogs map[string]string, checkpoints []*datapb.CheckPoint,
	startPositions []*datapb.SegmentStartPosition) error {
	m.Lock()
	defer m.Unlock()
	kv := make(map[string]string)
	for k, v := range binlogs {
		kv[k] = v
	}
	if flushed {
		m.segments.SetState(segID, commonpb.SegmentState_Flushing)
	}

	modSegments := make([]UniqueID, 0)
	for _, pos := range startPositions {
		if len(pos.GetStartPosition().GetMsgID()) != 0 {
			continue
		}
		if segment := m.segments.GetSegment(pos.GetSegmentID()); segment != nil {
			m.segments.SetStartPosition(pos.GetSegmentID(), pos.GetStartPosition())
			modSegments = append(modSegments, pos.GetSegmentID())
		}
	}

	for _, cp := range checkpoints {
		if segment := m.segments.GetSegment(cp.GetSegmentID()); segment != nil {
			if segment.DmlPosition != nil && segment.DmlPosition.Timestamp >= cp.Position.Timestamp {
				// segment position in etcd is larger than checkpoint, then dont change it
				continue
			}
			m.segments.SetDmlPositino(cp.GetSegmentID(), cp.GetPosition())
			m.segments.SetRowCount(cp.GetSegmentID(), cp.GetNumOfRows())
			modSegments = append(modSegments, segment.GetID())
		}
	}

	for _, id := range modSegments {
		if segment := m.segments.GetSegment(id); segment != nil {
			segBytes := proto.MarshalTextString(segment)
			key := buildSegmentPath(segment.GetCollectionID(), segment.GetPartitionID(), segment.GetID())
			kv[key] = segBytes
		}
	}

	if err := m.saveKvTxn(kv); err != nil {
		return err
	}
	return nil
}

func (m *meta) GetSegmentsByChannel(dmlCh string) []*datapb.SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	infos := make([]*datapb.SegmentInfo, 0)
	segments := m.segments.GetSegments()
	for _, segment := range segments {
		if segment.InsertChannel != dmlCh {
			continue
		}
		infos = append(infos, segment)
	}
	return infos
}

func (m *meta) GetSegmentsOfCollection(collectionID UniqueID) []UniqueID {
	m.RLock()
	defer m.RUnlock()
	ret := make([]UniqueID, 0)
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if info.CollectionID == collectionID {
			ret = append(ret, info.ID)
		}
	}
	return ret
}

func (m *meta) GetSegmentsOfPartition(collectionID, partitionID UniqueID) []UniqueID {
	m.RLock()
	defer m.RUnlock()
	ret := make([]UniqueID, 0)
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if info.CollectionID == collectionID && info.PartitionID == partitionID {
			ret = append(ret, info.ID)
		}
	}
	return ret
}

func (m *meta) GetNumRowsOfPartition(collectionID UniqueID, partitionID UniqueID) int64 {
	m.RLock()
	defer m.RUnlock()
	var ret int64 = 0
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if info.CollectionID == collectionID && info.PartitionID == partitionID {
			ret += info.NumOfRows
		}
	}
	return ret
}

func (m *meta) GetUnFlushedSegments() []*datapb.SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	ret := make([]*datapb.SegmentInfo, 0)
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if info.State != commonpb.SegmentState_Flushing && info.State != commonpb.SegmentState_Flushed {
			ret = append(ret, info)
		}
	}
	return ret
}

func (m *meta) GetFlushingSegments() []*datapb.SegmentInfo {
	m.RLock()
	defer m.RUnlock()
	ret := make([]*datapb.SegmentInfo, 0)
	segments := m.segments.GetSegments()
	for _, info := range segments {
		if info.State == commonpb.SegmentState_Flushing {
			ret = append(ret, info)
		}
	}
	return ret
}

func (m *meta) saveSegmentInfo(segment *datapb.SegmentInfo) error {
	segBytes := proto.MarshalTextString(segment)

	key := buildSegmentPath(segment.GetCollectionID(), segment.GetPartitionID(), segment.GetID())
	return m.client.Save(key, segBytes)
}

func (m *meta) removeSegmentInfo(segment *datapb.SegmentInfo) error {
	key := buildSegmentPath(segment.GetCollectionID(), segment.GetPartitionID(), segment.GetID())
	return m.client.Remove(key)
}

func (m *meta) saveKvTxn(kv map[string]string) error {
	return m.client.MultiSave(kv)
}

func buildSegmentPath(collectionID UniqueID, partitionID UniqueID, segmentID UniqueID) string {
	return fmt.Sprintf("%s/%d/%d/%d", segmentPrefix, collectionID, partitionID, segmentID)
}

func buildCollectionPath(collectionID UniqueID) string {
	return fmt.Sprintf("%s/%d/", segmentPrefix, collectionID)
}

func buildPartitionPath(collectionID UniqueID, partitionID UniqueID) string {
	return fmt.Sprintf("%s/%d/%d/", segmentPrefix, collectionID, partitionID)
}

func buildSegment(collectionID UniqueID, partitionID UniqueID, segmentID UniqueID, channelName string) *datapb.SegmentInfo {
	return &datapb.SegmentInfo{
		ID:            segmentID,
		CollectionID:  collectionID,
		PartitionID:   partitionID,
		InsertChannel: channelName,
		NumOfRows:     0,
		State:         commonpb.SegmentState_Growing,
	}
}
