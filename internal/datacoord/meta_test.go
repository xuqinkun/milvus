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
	"testing"

	"github.com/golang/protobuf/proto"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/stretchr/testify/assert"
)

func TestMeta_Basic(t *testing.T) {
	const collID = UniqueID(0)
	const partID0 = UniqueID(100)
	const partID1 = UniqueID(101)
	const channelName = "c1"

	mockAllocator := newMockAllocator()
	meta, err := newMemoryMeta(mockAllocator)
	assert.Nil(t, err)

	testSchema := newTestSchema()
	collInfo := &datapb.CollectionInfo{
		ID:         collID,
		Schema:     testSchema,
		Partitions: []UniqueID{partID0, partID1},
	}
	collInfoWoPartition := &datapb.CollectionInfo{
		ID:         collID,
		Schema:     testSchema,
		Partitions: []UniqueID{},
	}

	t.Run("Test Collection", func(t *testing.T) {
		meta.AddCollection(collInfo)
		// check has collection
		collInfo := meta.GetCollection(collID)
		assert.NotNil(t, collInfo)

		// check partition info
		assert.EqualValues(t, collID, collInfo.ID)
		assert.EqualValues(t, testSchema, collInfo.Schema)
		assert.EqualValues(t, 2, len(collInfo.Partitions))
		assert.EqualValues(t, partID0, collInfo.Partitions[0])
		assert.EqualValues(t, partID1, collInfo.Partitions[1])
	})

	t.Run("Test Segment", func(t *testing.T) {
		meta.AddCollection(collInfoWoPartition)
		// create seg0 for partition0, seg0/seg1 for partition1
		segID0_0, err := mockAllocator.allocID()
		assert.Nil(t, err)
		segInfo0_0 := buildSegment(collID, partID0, segID0_0, channelName)
		segID1_0, err := mockAllocator.allocID()
		assert.Nil(t, err)
		segInfo1_0 := buildSegment(collID, partID1, segID1_0, channelName)
		segID1_1, err := mockAllocator.allocID()
		assert.Nil(t, err)
		segInfo1_1 := buildSegment(collID, partID1, segID1_1, channelName)

		// check AddSegment
		err = meta.AddSegment(segInfo0_0)
		assert.Nil(t, err)
		err = meta.AddSegment(segInfo1_0)
		assert.Nil(t, err)
		err = meta.AddSegment(segInfo1_1)
		assert.Nil(t, err)

		// check GetSegment
		info0_0 := meta.GetSegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.True(t, proto.Equal(info0_0, segInfo0_0))
		info1_0 := meta.GetSegment(segID1_0)
		assert.NotNil(t, info1_0)
		assert.True(t, proto.Equal(info1_0, segInfo1_0))

		// check GetSegmentsOfCollection
		segIDs := meta.GetSegmentsOfCollection(collID)
		assert.EqualValues(t, 3, len(segIDs))
		assert.Contains(t, segIDs, segID0_0)
		assert.Contains(t, segIDs, segID1_0)
		assert.Contains(t, segIDs, segID1_1)

		// check GetSegmentsOfPartition
		segIDs = meta.GetSegmentsOfPartition(collID, partID0)
		assert.EqualValues(t, 1, len(segIDs))
		assert.Contains(t, segIDs, segID0_0)
		segIDs = meta.GetSegmentsOfPartition(collID, partID1)
		assert.EqualValues(t, 2, len(segIDs))
		assert.Contains(t, segIDs, segID1_0)
		assert.Contains(t, segIDs, segID1_1)

		// check DropSegment
		err = meta.DropSegment(segID1_0)
		assert.Nil(t, err)
		segIDs = meta.GetSegmentsOfPartition(collID, partID1)
		assert.EqualValues(t, 1, len(segIDs))
		assert.Contains(t, segIDs, segID1_1)

		err = meta.SetState(segID0_0, commonpb.SegmentState_Sealed)
		assert.Nil(t, err)
		err = meta.SetState(segID0_0, commonpb.SegmentState_Flushed)
		assert.Nil(t, err)

		info0_0 = meta.GetSegment(segID0_0)
		assert.NotNil(t, info0_0)
		assert.EqualValues(t, commonpb.SegmentState_Flushed, info0_0.State)
	})

	t.Run("Test GetCount", func(t *testing.T) {
		const rowCount0 = 100
		const rowCount1 = 300
		const dim = 1024

		// no segment
		nums := meta.GetNumRowsOfCollection(collID)
		assert.EqualValues(t, 0, nums)

		// add seg1 with 100 rows
		segID0, err := mockAllocator.allocID()
		assert.Nil(t, err)
		segInfo0 := buildSegment(collID, partID0, segID0, channelName)
		segInfo0.NumOfRows = rowCount0
		err = meta.AddSegment(segInfo0)
		assert.Nil(t, err)

		// add seg2 with 300 rows
		segID1, err := mockAllocator.allocID()
		assert.Nil(t, err)
		segInfo1 := buildSegment(collID, partID0, segID1, channelName)
		segInfo1.NumOfRows = rowCount1
		err = meta.AddSegment(segInfo1)
		assert.Nil(t, err)

		// check partition/collection statistics
		nums = meta.GetNumRowsOfPartition(collID, partID0)
		assert.EqualValues(t, (rowCount0 + rowCount1), nums)
		nums = meta.GetNumRowsOfCollection(collID)
		assert.EqualValues(t, (rowCount0 + rowCount1), nums)
	})
}

func TestGetUnFlushedSegments(t *testing.T) {
	mockAllocator := newMockAllocator()
	meta, err := newMemoryMeta(mockAllocator)
	assert.Nil(t, err)
	err = meta.AddSegment(&datapb.SegmentInfo{
		ID:           0,
		CollectionID: 0,
		PartitionID:  0,
		State:        commonpb.SegmentState_Growing,
	})
	assert.Nil(t, err)
	err = meta.AddSegment(&datapb.SegmentInfo{
		ID:           1,
		CollectionID: 0,
		PartitionID:  0,
		State:        commonpb.SegmentState_Flushed,
	})
	assert.Nil(t, err)

	segments := meta.GetUnFlushedSegments()
	assert.Nil(t, err)

	assert.EqualValues(t, 1, len(segments))
	assert.EqualValues(t, 0, segments[0].ID)
	assert.NotEqualValues(t, commonpb.SegmentState_Flushed, segments[0].State)
}
