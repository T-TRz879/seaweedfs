package broker

import (
	"context"
	"fmt"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mq/topic"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/mq_pb"
	"google.golang.org/grpc/peer"
	"io"
	"math/rand"
	"net"
)

// PUB
// 1. gRPC API to configure a topic
//    1.1 create a topic with existing partition count
//    1.2 assign partitions to brokers
// 2. gRPC API to lookup topic partitions
// 3. gRPC API to publish by topic partitions

// SUB
// 1. gRPC API to lookup a topic partitions

// Re-balance topic partitions for publishing
//   1. collect stats from all the brokers
//   2. Rebalance and configure new generation of partitions on brokers
//   3. Tell brokers to close current gneration of publishing.
// Publishers needs to lookup again and publish to the new generation of partitions.

// Re-balance topic partitions for subscribing
//   1. collect stats from all the brokers
// Subscribers needs to listen for new partitions and connect to the brokers.
// Each subscription may not get data. It can act as a backup.

func (b *MessageQueueBroker) PublishMessage(stream mq_pb.SeaweedMessaging_PublishMessageServer) error {
	// 1. write to the volume server
	// 2. find the topic metadata owning filer
	// 3. write to the filer

	var localTopicPartition *topic.LocalPartition
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	response := &mq_pb.PublishMessageResponse{}
	// TODO check whether current broker should be the leader for the topic partition
	ackInterval := 1
	initMessage := req.GetInit()
	if initMessage == nil {
		response.Error = fmt.Sprintf("missing init message")
		glog.Errorf("missing init message")
		return stream.Send(response)
	}

	// get or generate a local partition
	t, p := topic.FromPbTopic(initMessage.Topic), topic.FromPbPartition(initMessage.Partition)
	conf, readConfErr := b.readTopicConfFromFiler(t)
	if readConfErr != nil {
		response.Error = fmt.Sprintf("topic %v not found: %v", initMessage.Topic, readConfErr)
		glog.Errorf("topic %v not found: %v", initMessage.Topic, readConfErr)
		return stream.Send(response)
	}
	localTopicPartition, _, err = b.GetOrGenLocalPartition(t, p, conf)
	if err != nil {
		response.Error = fmt.Sprintf("topic %v partition %v not setup", initMessage.Topic, initMessage.Partition)
		glog.Errorf("topic %v partition %v not setup", initMessage.Topic, initMessage.Partition)
		return stream.Send(response)
	}
	ackInterval = int(initMessage.AckInterval)

	// connect to follower brokers
	if localTopicPartition.FollowerStream == nil && len(initMessage.FollowerBrokers) > 0 {
		follower := initMessage.FollowerBrokers[0]
		ctx := stream.Context()
		localTopicPartition.GrpcConnection, err = pb.GrpcDial(ctx, follower, true, b.grpcDialOption)
		if err != nil {
			response.Error = fmt.Sprintf("fail to dial %s: %v", follower, err)
			glog.Errorf("fail to dial %s: %v", follower, err)
			return stream.Send(response)
		}
		followerClient := mq_pb.NewSeaweedMessagingClient(localTopicPartition.GrpcConnection)
		localTopicPartition.FollowerStream, err = followerClient.PublishFollowMe(ctx)
		if err != nil {
			response.Error = fmt.Sprintf("fail to create publish client: %v", err)
			glog.Errorf("fail to create publish client: %v", err)
			return stream.Send(response)
		}
		if err = localTopicPartition.FollowerStream.Send(&mq_pb.PublishFollowMeRequest{
			Message: &mq_pb.PublishFollowMeRequest_Init{
				Init: &mq_pb.PublishFollowMeRequest_InitMessage{
					Topic:      initMessage.Topic,
					Partition:  initMessage.Partition,
				},
			},
		}); err != nil {
			return err
		}
	}

	// process each published messages
	clientName := fmt.Sprintf("%v-%4d/%s/%v", findClientAddress(stream.Context()), rand.Intn(10000), initMessage.Topic, initMessage.Partition)
	localTopicPartition.Publishers.AddPublisher(clientName, topic.NewLocalPublisher())

	ackCounter := 0
	var ackSequence int64
	defer func() {
		if localTopicPartition.FollowerStream == nil {
			// remove the publisher
			localTopicPartition.Publishers.RemovePublisher(clientName)
			glog.V(0).Infof("topic %v partition %v published %d messges Publisher:%d Subscriber:%d", initMessage.Topic, initMessage.Partition, ackSequence, localTopicPartition.Publishers.Size(), localTopicPartition.Subscribers.Size())
			if localTopicPartition.MaybeShutdownLocalPartition() {
				b.localTopicManager.RemoveTopicPartition(t, p)
			}
		}
	}()

	if localTopicPartition.FollowerStream != nil {
		go func() {
			defer func() {
				println("stop receiving ack from follower")

				// remove the publisher
				localTopicPartition.Publishers.RemovePublisher(clientName)
				glog.V(0).Infof("topic %v partition %v published %d messges Publisher:%d Subscriber:%d", initMessage.Topic, initMessage.Partition, ackSequence, localTopicPartition.Publishers.Size(), localTopicPartition.Subscribers.Size())
				if localTopicPartition.MaybeShutdownLocalPartition() {
					b.localTopicManager.RemoveTopicPartition(t, p)
				}
				println("closing grpcConnection to follower")
				localTopicPartition.GrpcConnection.Close()
			}()

			for {
				ack, err := localTopicPartition.FollowerStream.Recv()
				if err != nil {
					glog.Errorf("Error receiving response: %v", err)
					return
				}
				ackSequence = ack.AckTsNs
				println("recv ack", ack.AckTsNs)
				if err := stream.Send(&mq_pb.PublishMessageResponse{
					AckSequence: ack.AckTsNs,
				}); err != nil {
					glog.Errorf("Error sending response %v: %v", ack, err)
					return
				}
			}
		}()
	}

	// send a hello message
	stream.Send(&mq_pb.PublishMessageResponse{})

	var receivedSequence, acknowledgedSequence  int64

	defer func() {
		if localTopicPartition.FollowerStream != nil {
			//if err := followerStream.CloseSend(); err != nil {
			//	glog.Errorf("Error closing follower stream: %v", err)
			//}
		} else {
			if acknowledgedSequence < receivedSequence {
				acknowledgedSequence = receivedSequence
				response := &mq_pb.PublishMessageResponse{
					AckSequence: acknowledgedSequence,
				}
				if err := stream.Send(response); err != nil {
					glog.Errorf("Error sending response %v: %v", response, err)
				}
			}
		}
	}()

	// process each published messages
	for {
		// receive a message
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			glog.V(0).Infof("topic %v partition %v publish stream error: %v", initMessage.Topic, initMessage.Partition, err)
			return err
		}

		// Process the received message
		dataMessage := req.GetData()
		if dataMessage == nil {
			continue
		}

		// send to the local partition
		localTopicPartition.Publish(dataMessage)
		receivedSequence = dataMessage.TsNs

		// maybe send to the follower
		if localTopicPartition.FollowerStream != nil {
			println("recv", string(dataMessage.Key), dataMessage.TsNs)
			if followErr := localTopicPartition.FollowerStream.Send(&mq_pb.PublishFollowMeRequest{
				Message: &mq_pb.PublishFollowMeRequest_Data{
					Data: dataMessage,
				},
			}); followErr != nil {
				return followErr
			}
		} else {
			ackCounter++
			if ackCounter >= ackInterval {
				ackCounter = 0
				// send back the ack directly
				acknowledgedSequence = receivedSequence
				response := &mq_pb.PublishMessageResponse{
					AckSequence: acknowledgedSequence,
				}
				if err := stream.Send(response); err != nil {
					glog.Errorf("Error sending response %v: %v", response, err)
				}
			}
		}
	}

	if localTopicPartition.FollowerStream != nil {
		// send close to the follower
		if followErr := localTopicPartition.FollowerStream.Send(&mq_pb.PublishFollowMeRequest{
			Message: &mq_pb.PublishFollowMeRequest_Close{
				Close: &mq_pb.PublishFollowMeRequest_CloseMessage{},
			},
		}); followErr != nil {
			return followErr
		}
		println("closing follower stream")

		//if err := followerStream.CloseSend(); err != nil {
		//	glog.Errorf("Error closing follower stream: %v", err)
		//}
	}

	glog.V(0).Infof("topic %v partition %v publish stream closed.", initMessage.Topic, initMessage.Partition)

	return nil
}

// duplicated from master_grpc_server.go
func findClientAddress(ctx context.Context) string {
	// fmt.Printf("FromContext %+v\n", ctx)
	pr, ok := peer.FromContext(ctx)
	if !ok {
		glog.Error("failed to get peer from ctx")
		return ""
	}
	if pr.Addr == net.Addr(nil) {
		glog.Error("failed to get peer address")
		return ""
	}
	return pr.Addr.String()
}
