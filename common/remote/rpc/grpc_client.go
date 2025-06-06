/*
 * Copyright 1999-2020 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package rpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/credentials"

	"github.com/pkg/errors"

	"github.com/dbsyk/nacos-sdk-go/v2/common/remote/rpc/rpc_request"
	"github.com/dbsyk/nacos-sdk-go/v2/common/remote/rpc/rpc_response"

	nacos_grpc_service "github.com/dbsyk/nacos-sdk-go/v2/api/grpc"
	"github.com/dbsyk/nacos-sdk-go/v2/common/constant"
	"github.com/dbsyk/nacos-sdk-go/v2/common/logger"
	"github.com/dbsyk/nacos-sdk-go/v2/common/nacos_server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type GrpcClient struct {
	*RpcClient
	*constant.TLSConfig
}

func NewGrpcClient(ctx context.Context, clientName string, nacosServer *nacos_server.NacosServer, tlsConfig *constant.TLSConfig) *GrpcClient {
	rpcClient := &GrpcClient{
		&RpcClient{
			ctx:              ctx,
			name:             clientName,
			labels:           make(map[string]string, 8),
			rpcClientStatus:  INITIALIZED,
			eventChan:        make(chan ConnectionEvent, 1),
			reconnectionChan: make(chan ReconnectContext, 1),
			nacosServer:      nacosServer,
			mux:              new(sync.Mutex),
		}, tlsConfig,
	}
	rpcClient.RpcClient.lastActiveTimestamp.Store(time.Now())
	rpcClient.executeClient = rpcClient
	listeners := make([]IConnectionEventListener, 0, 8)
	rpcClient.connectionEventListeners.Store(listeners)
	return rpcClient
}

func getMaxCallRecvMsgSize() int {
	maxCallRecvMsgSizeInt, err := strconv.Atoi(os.Getenv("nacos.remote.client.grpc.maxinbound.message.size"))
	if err != nil {
		return 10 * 1024 * 1024
	}
	return maxCallRecvMsgSizeInt
}

func getInitialWindowSize() int32 {
	initialWindowSize, err := strconv.Atoi(os.Getenv("nacos.remote.client.grpc.initial.window.size"))
	if err != nil {
		return 10 * 1024 * 1024
	}
	return int32(initialWindowSize)
}

func getInitialConnWindowSize() int32 {
	initialConnWindowSize, err := strconv.Atoi(os.Getenv("nacos.remote.client.grpc.initial.conn.window.size"))
	if err != nil {
		return 10 * 1024 * 1024
	}
	return int32(initialConnWindowSize)
}

func getTLSCredentials(tlsConfig *constant.TLSConfig, serverInfo ServerInfo) credentials.TransportCredentials {

	logger.Infof("build tls config for connecting to server %s,tlsConfig = %s", serverInfo.serverIp, tlsConfig)

	certPool, err := x509.SystemCertPool()
	if err != nil {
		log.Fatalf("load root cert pool fail : %v", err)
	}
	if len(tlsConfig.CaFile) != 0 {
		cert, err := os.ReadFile(tlsConfig.CaFile)
		if err != nil {
			fmt.Errorf("err, %v", err)
		}
		if ok := certPool.AppendCertsFromPEM(cert); !ok {
			fmt.Errorf("failed to append ca certs")
		}
	}

	config := tls.Config{
		InsecureSkipVerify: tlsConfig.TrustAll,
		RootCAs:            certPool,
		Certificates:       []tls.Certificate{},
	}
	if len(tlsConfig.CertFile) != 0 && len(tlsConfig.KeyFile) != 0 {
		cert, err := tls.LoadX509KeyPair(tlsConfig.CertFile, tlsConfig.KeyFile)

		if err != nil {
			log.Fatalf("load cert fail : %v", err)
		}
		config.Certificates = append(config.Certificates, cert)
	}
	credentials := credentials.NewTLS(&config)
	return credentials
}

func getInitialGrpcTimeout() int32 {
	initialGrpcTimeout, err := strconv.Atoi(os.Getenv("nacos.remote.client.grpc.timeout"))
	if err != nil {
		return constant.DEFAULT_TIMEOUT_MILLS
	}
	return int32(initialGrpcTimeout)
}

func getKeepAliveTimeMillis() keepalive.ClientParameters {
	keepAliveTimeMillisInt, err := strconv.Atoi(os.Getenv("nacos.remote.grpc.keep.alive.millis"))
	var keepAliveTime time.Duration
	if err != nil {
		keepAliveTime = 60 * 1000 * time.Millisecond
	} else {
		keepAliveTime = time.Duration(keepAliveTimeMillisInt) * time.Millisecond
	}
	return keepalive.ClientParameters{
		Time:                keepAliveTime,    // send pings every 60 seconds if there is no activity
		Timeout:             20 * time.Second, // wait 20 second for ping ack before considering the connection dead
		PermitWithoutStream: true,             // send pings even without active streams
	}
}

func (c *GrpcClient) createNewConnection(serverInfo ServerInfo) (*grpc.ClientConn, error) {
	var opts []grpc.DialOption
	opts = append(opts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(getMaxCallRecvMsgSize())))
	opts = append(opts, grpc.WithKeepaliveParams(getKeepAliveTimeMillis()))
	opts = append(opts, grpc.WithInsecure())
	opts = append(opts, grpc.WithInitialWindowSize(getInitialWindowSize()))
	opts = append(opts, grpc.WithInitialConnWindowSize(getInitialConnWindowSize()))
	c.getEnvTLSConfig(c.TLSConfig)
	if c.TLSConfig.Enable {
		logger.Infof(" tls enable ,trying to connection to server %s with tls config %s", serverInfo.serverIp, c.TLSConfig)
		opts = append(opts, grpc.WithTransportCredentials(getTLSCredentials(c.TLSConfig, serverInfo)))
	}
	rpcPort := serverInfo.serverGrpcPort
	if rpcPort == 0 {
		rpcPort = serverInfo.serverPort + c.rpcPortOffset()
	}
	return grpc.Dial(serverInfo.serverIp+":"+strconv.FormatUint(rpcPort, 10), opts...)

}

func (c *GrpcClient) getEnvTLSConfig(config *constant.TLSConfig) {
	logger.Infof("check tls config ", config)

	if config.Appointed == true {
		return
	}
	logger.Infof("try to get tls config from env")

	enableTls, err := strconv.ParseBool(os.Getenv("nacos_remote_client_rpc_tls_enable"))
	if err == nil {
		config.Enable = enableTls
		logger.Infof("get tls config from env ，key = enableTls value = %s", enableTls)
	}

	if enableTls != true {
		logger.Infof(" tls config from env is not enable")
		return
	}
	trustAll, err := strconv.ParseBool(os.Getenv("nacos_remote_client_rpc_tls_trustAll"))
	if err == nil {
		config.TrustAll = trustAll
		logger.Infof("get tls config from env ，key = trustAll value = %s", trustAll)
	}

	config.CaFile = os.Getenv("nacos_remote_client_rpc_tls_trustCollectionChainPath")
	logger.Infof("get tls config from env ，key = trustCollectionChainPath value = %s", config.CaFile)
	config.CertFile = os.Getenv("nacos_remote_client_rpc_tls_certChainFile")
	logger.Infof("get tls config from env ，key = certChainFile value = %s", config.CertFile)
	config.KeyFile = os.Getenv("nacos_remote_client_rpc_tls_certPrivateKey")
	logger.Infof("get tls config from env ，key = certPrivateKey value = %s", config.KeyFile)

}

func (c *GrpcClient) connectToServer(serverInfo ServerInfo) (IConnection, error) {
	var client nacos_grpc_service.RequestClient
	var biStreamClient nacos_grpc_service.BiRequestStreamClient

	conn, err := c.createNewConnection(serverInfo)
	if err != nil {
		return nil, errors.Errorf("grpc create new connection failed , err:%v", err)
	}

	client = nacos_grpc_service.NewRequestClient(conn)
	response, err := serverCheck(client)
	if err != nil {
		_ = conn.Close()
		return nil, errors.Errorf("server check request failed , err:%v", err)
	}
	serverCheckResponse := response.(*rpc_response.ServerCheckResponse)

	biStreamClient = nacos_grpc_service.NewBiRequestStreamClient(conn)
	biStreamRequestClient, err := biStreamClient.RequestBiStream(context.Background())
	if err != nil {
		return nil, errors.Errorf("create biStreamRequestClient failed , err:%v", err)
	}
	grpcConn := NewGrpcConnection(serverInfo, serverCheckResponse.ConnectionId, conn, client, biStreamRequestClient)
	c.bindBiRequestStream(biStreamRequestClient, grpcConn)
	err = c.sendConnectionSetupRequest(grpcConn)
	return grpcConn, err
}

func (c *GrpcClient) sendConnectionSetupRequest(grpcConn *GrpcConnection) error {
	csr := rpc_request.NewConnectionSetupRequest()
	csr.ClientVersion = constant.CLIENT_VERSION
	csr.Tenant = c.Tenant
	csr.Labels = c.labels
	csr.ClientAbilities = c.clientAbilities
	err := grpcConn.biStreamSend(convertRequest(csr))
	if err != nil {
		logger.Warnf("send connectionSetupRequest error:%v", err)
	}
	time.Sleep(100 * time.Millisecond)
	return err
}

func (c *GrpcClient) getConnectionType() ConnectionType {
	return GRPC
}

func (c *GrpcClient) rpcPortOffset() uint64 {
	return constant.RpcPortOffset
}

func (c *GrpcClient) bindBiRequestStream(streamClient nacos_grpc_service.BiRequestStream_RequestBiStreamClient, grpcConn *GrpcConnection) {
	go func() {
		for {
			select {
			case <-streamClient.Context().Done():
				logger.Warnf("connectionId %s stream client close", grpcConn.getConnectionId())
				return
			default:
				payload, err := streamClient.Recv()
				if err != nil {
					running := c.IsRunning()
					abandon := grpcConn.getAbandon()
					if c.IsRunning() && !abandon {
						if err == io.EOF {
							logger.Infof("connectionId %s request stream onCompleted, switch server", grpcConn.getConnectionId())
						} else {
							logger.Errorf("connectionId %s request stream error, switch server, error=%v", grpcConn.getConnectionId(), err)
						}
						if atomic.CompareAndSwapInt32((*int32)(&c.rpcClientStatus), int32(RUNNING), int32(UNHEALTHY)) {
							c.switchServerAsync(ServerInfo{}, false)
							return
						}
					} else {
						logger.Errorf("connectionId %s received error event, isRunning:%v, isAbandon=%v, error=%v", grpcConn.getConnectionId(), running, abandon, err)
						return
					}
				} else {
					c.handleServerRequest(payload, grpcConn)
				}
			}
		}
	}()
}

func serverCheck(client nacos_grpc_service.RequestClient) (rpc_response.IResponse, error) {
	var response rpc_response.ServerCheckResponse
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(getInitialGrpcTimeout())*time.Millisecond)
	defer cancel()
	for i := 0; i <= 30; i++ {
		payload, err := client.Request(ctx, convertRequest(rpc_request.NewServerCheckRequest()))
		if err != nil {
			return nil, err
		}
		err = json.Unmarshal(payload.GetBody().Value, &response)
		if err != nil {
			return nil, err
		}
		// check if the server is ready, if not, wait 1 second and try again
		if response.GetErrorCode() >= 300 && response.GetErrorCode() < 400 {
			// if we wait 30 second, but the server is not ready,then throw this error
			if i == 30 {
				return nil, errors.New("the nacos server is not ready to work in 30 seconds, connect to server failed")
			}
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	return &response, nil
}

func (c *GrpcClient) handleServerRequest(p *nacos_grpc_service.Payload, grpcConn *GrpcConnection) {
	client := c.GetRpcClient()
	payLoadType := p.GetMetadata().GetType()

	handlerMapping, ok := client.serverRequestHandlerMapping.Load(payLoadType)
	if !ok {
		logger.Errorf("%s Unsupported payload type", grpcConn.getConnectionId())
		return
	}

	mapping := handlerMapping.(ServerRequestHandlerMapping)

	serverRequest := mapping.serverRequest()
	err := json.Unmarshal(p.GetBody().Value, serverRequest)
	if err != nil {
		logger.Errorf("%s Fail to json Unmarshal for request:%s, ackId->%s", grpcConn.getConnectionId(),
			serverRequest.GetRequestType(), serverRequest.GetRequestId())
		return
	}

	serverRequest.PutAllHeaders(p.GetMetadata().Headers)

	response := mapping.handler.RequestReply(serverRequest, client)
	if response == nil {
		logger.Warnf("%s Fail to process server request, ackId->%s", grpcConn.getConnectionId(),
			serverRequest.GetRequestId())
		return
	}
	response.SetRequestId(serverRequest.GetRequestId())
	err = grpcConn.biStreamSend(convertResponse(response))
	if err != nil && err != io.EOF {
		logger.Warnf("%s Fail to send response:%s,ackId->%s", grpcConn.getConnectionId(),
			response.GetResponseType(), serverRequest.GetRequestId())
	}
}
