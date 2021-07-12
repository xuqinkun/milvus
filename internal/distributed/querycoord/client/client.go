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

package grpcquerycoordclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/milvus-io/milvus/internal/util/retry"
	"github.com/milvus-io/milvus/internal/util/sessionutil"
	"github.com/milvus-io/milvus/internal/util/trace"
	"github.com/milvus-io/milvus/internal/util/typeutil"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
)

type Client struct {
	ctx    context.Context
	cancel context.CancelFunc

	grpcClient querypb.QueryCoordClient
	conn       *grpc.ClientConn

	sess *sessionutil.Session
	addr string
}

func getQueryCoordAddress(sess *sessionutil.Session) (string, error) {
	key := typeutil.QueryCoordRole
	msess, _, err := sess.GetSessions(key)
	if err != nil {
		log.Debug("QueryCoordClient GetSessions failed", zap.Error(err))
		return "", err
	}
	ms, ok := msess[key]
	if !ok {
		log.Debug("QueryCoordClient msess key not existed", zap.Any("key", key))
		return "", fmt.Errorf("number of querycoord is incorrect, %d", len(msess))
	}
	return ms.Address, nil
}

// NewClient creates a client for QueryCoord grpc call.
func NewClient(ctx context.Context, metaRoot string, etcdEndpoints []string) (*Client, error) {
	sess := sessionutil.NewSession(ctx, metaRoot, etcdEndpoints)
	if sess == nil {
		err := fmt.Errorf("new session error, maybe can not connect to etcd")
		log.Debug("QueryCoordClient NewClient failed", zap.Error(err))
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &Client{
		ctx:    ctx,
		cancel: cancel,
		sess:   sess,
	}, nil
}

func (c *Client) Init() error {
	return c.connect(retry.Attempts(20))
}

func (c *Client) connect(retryOptions ...retry.Option) error {
	var err error
	connectQueryCoordAddressFn := func() error {
		c.addr, err = getQueryCoordAddress(c.sess)
		if err != nil {
			log.Debug("QueryCoordClient getQueryCoordAddress failed", zap.Error(err))
			return err
		}
		opts := trace.GetInterceptorOpts()
		log.Debug("QueryCoordClient try reconnect ", zap.String("address", c.addr))
		conn, err := grpc.DialContext(c.ctx, c.addr,
			grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(2*time.Second),
			grpc.WithUnaryInterceptor(
				grpc_middleware.ChainUnaryClient(
					grpc_retry.UnaryClientInterceptor(
						grpc_retry.WithMax(3),
						grpc_retry.WithPerRetryTimeout(time.Second*10),
						grpc_retry.WithCodes(codes.Aborted, codes.Unavailable),
					),
					grpc_opentracing.UnaryClientInterceptor(opts...),
				)),
			grpc.WithStreamInterceptor(
				grpc_middleware.ChainStreamClient(
					grpc_retry.StreamClientInterceptor(
						grpc_retry.WithMax(3),
						grpc_retry.WithPerRetryTimeout(time.Second*10),
						grpc_retry.WithCodes(codes.Aborted, codes.Unavailable),
					),
					grpc_opentracing.StreamClientInterceptor(opts...),
				)),
		)
		if err != nil {
			return err
		}
		c.conn = conn
		return nil
	}

	err = retry.Do(c.ctx, connectQueryCoordAddressFn, retryOptions...)
	if err != nil {
		log.Debug("QueryCoordClient try reconnect failed", zap.Error(err))
		return err
	}
	log.Debug("QueryCoordClient try reconnect success")
	c.grpcClient = querypb.NewQueryCoordClient(c.conn)
	return nil
}

func (c *Client) recall(caller func() (interface{}, error)) (interface{}, error) {
	ret, err := caller()
	if err == nil {
		return ret, nil
	}
	log.Debug("QueryCoord Client grpc error", zap.Error(err))
	err = c.connect()
	if err != nil {
		return ret, errors.New("Connect to querycoord failed with error:\n" + err.Error())
	}
	ret, err = caller()
	if err == nil {
		return ret, nil
	}
	return ret, err
}

func (c *Client) Start() error {
	return nil
}

func (c *Client) Stop() error {
	c.cancel()
	return c.conn.Close()
}

// Register dummy
func (c *Client) Register() error {
	return nil
}

func (c *Client) GetComponentStates(ctx context.Context) (*internalpb.ComponentStates, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.GetComponentStates(ctx, &internalpb.GetComponentStatesRequest{})
	})
	return ret.(*internalpb.ComponentStates), err
}

func (c *Client) GetTimeTickChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.GetTimeTickChannel(ctx, &internalpb.GetTimeTickChannelRequest{})
	})
	return ret.(*milvuspb.StringResponse), err
}

func (c *Client) GetStatisticsChannel(ctx context.Context) (*milvuspb.StringResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.GetStatisticsChannel(ctx, &internalpb.GetStatisticsChannelRequest{})
	})
	return ret.(*milvuspb.StringResponse), err
}

func (c *Client) ShowCollections(ctx context.Context, req *querypb.ShowCollectionsRequest) (*querypb.ShowCollectionsResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.ShowCollections(ctx, req)
	})
	return ret.(*querypb.ShowCollectionsResponse), err
}

func (c *Client) LoadCollection(ctx context.Context, req *querypb.LoadCollectionRequest) (*commonpb.Status, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.LoadCollection(ctx, req)
	})
	return ret.(*commonpb.Status), err
}

func (c *Client) ReleaseCollection(ctx context.Context, req *querypb.ReleaseCollectionRequest) (*commonpb.Status, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.ReleaseCollection(ctx, req)
	})
	return ret.(*commonpb.Status), err
}

func (c *Client) ShowPartitions(ctx context.Context, req *querypb.ShowPartitionsRequest) (*querypb.ShowPartitionsResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.ShowPartitions(ctx, req)
	})
	return ret.(*querypb.ShowPartitionsResponse), err
}

func (c *Client) LoadPartitions(ctx context.Context, req *querypb.LoadPartitionsRequest) (*commonpb.Status, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.LoadPartitions(ctx, req)
	})
	return ret.(*commonpb.Status), err
}

func (c *Client) ReleasePartitions(ctx context.Context, req *querypb.ReleasePartitionsRequest) (*commonpb.Status, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.ReleasePartitions(ctx, req)
	})
	return ret.(*commonpb.Status), err
}

func (c *Client) CreateQueryChannel(ctx context.Context, req *querypb.CreateQueryChannelRequest) (*querypb.CreateQueryChannelResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.CreateQueryChannel(ctx, req)
	})
	return ret.(*querypb.CreateQueryChannelResponse), err
}

func (c *Client) GetPartitionStates(ctx context.Context, req *querypb.GetPartitionStatesRequest) (*querypb.GetPartitionStatesResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.GetPartitionStates(ctx, req)
	})
	return ret.(*querypb.GetPartitionStatesResponse), err
}

func (c *Client) GetSegmentInfo(ctx context.Context, req *querypb.GetSegmentInfoRequest) (*querypb.GetSegmentInfoResponse, error) {
	ret, err := c.recall(func() (interface{}, error) {
		return c.grpcClient.GetSegmentInfo(ctx, req)
	})
	return ret.(*querypb.GetSegmentInfoResponse), err
}
