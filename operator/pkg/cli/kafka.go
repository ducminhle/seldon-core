package cli

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/seldonio/seldon-core/operatorv2/scheduler/apis/mlops/scheduler"
	"github.com/seldonio/seldon-core/operatorv2/scheduler/apis/mlops/v2_dataplane"
	"google.golang.org/protobuf/proto"
)

const (
	SeldonPrefix      = "seldon"
	DefaultNamespace  = "default"
	InputsSpecifier   = "inputs"
	OutputsSpecifier  = "outputs"
	PipelineSpecifier = "pipeline"
	ModelSpecifier    = "model"
)

type KafkaClient struct {
	consumer        *kafka.Consumer
	schedulerClient *SchedulerClient
}

type PipelineTopics struct {
	pipeline string
	topics   []string
	tensor   string
}

func NewKafkaClient(kafkaBroker string, schedulerHost string) (*KafkaClient, error) {
	s1 := rand.NewSource(time.Now().UnixNano())
	r1 := rand.New(s1)
	consumerConfig := kafka.ConfigMap{
		"bootstrap.servers": kafkaBroker,
		"group.id":          fmt.Sprintf("seldon-cli-%d", r1.Int()),
		"auto.offset.reset": "largest",
	}
	consumer, err := kafka.NewConsumer(&consumerConfig)
	if err != nil {
		return nil, err
	}

	kc := &KafkaClient{
		consumer:        consumer,
		schedulerClient: NewSchedulerClient(schedulerHost),
	}
	return kc, nil
}

func (kc *KafkaClient) subscribeAndSetOffset(pipelineStep string, offset int64) error {

	md, err := kc.consumer.GetMetadata(&pipelineStep, false, 1000)
	if err != nil {
		return err
	}

	for _, partitionMeta := range md.Topics[pipelineStep].Partitions {
		err := kc.consumer.Assign([]kafka.TopicPartition{
			{
				Topic:     &pipelineStep,
				Partition: partitionMeta.ID,
				//Note will get more messages than requested when multiple partitions available
				Offset: kafka.OffsetTail(kafka.Offset(offset)),
			},
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func hasStep(stepName string, response *scheduler.PipelineStatusResponse) bool {
	version := response.Versions[len(response.Versions)-1]
	for _, step := range version.GetPipeline().Steps {
		if step.Name == stepName {
			return true
		}
	}
	return false
}

func createPipelineTopics(pipelineSpec string, response *scheduler.PipelineStatusResponse) (*PipelineTopics, error) {
	parts := strings.Split(pipelineSpec, ".")
	switch len(parts) {
	case 1: //Just pipeline - show all steps and pipeline itself
		var topics []string
		for _, step := range response.Versions[len(response.Versions)-1].Pipeline.Steps {
			topics = append(topics, fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, step.Name, InputsSpecifier))
			topics = append(topics, fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, step.Name, OutputsSpecifier))
		}
		topics = append(topics, fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, PipelineSpecifier, parts[0], InputsSpecifier))
		topics = append(topics, fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, PipelineSpecifier, parts[0], OutputsSpecifier))
		return &PipelineTopics{
			pipeline: pipelineSpec,
			topics:   topics,
		}, nil
	case 2:
		if parts[1] == InputsSpecifier || parts[1] == OutputsSpecifier {
			return &PipelineTopics{
				pipeline: parts[0],
				topics:   []string{fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, PipelineSpecifier, parts[0], parts[1])},
			}, nil
		} else {
			if hasStep(parts[1], response) {
				return &PipelineTopics{
					pipeline: parts[0],
					topics: []string{
						fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, parts[1], InputsSpecifier),
						fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, parts[1], OutputsSpecifier),
					},
				}, nil
			} else {
				return nil, fmt.Errorf("Failed to find step with name %s in pipeline %s", parts[1], parts[0])
			}
		}
	case 3:
		if hasStep(parts[1], response) {
			if parts[2] == InputsSpecifier || parts[2] == OutputsSpecifier {
				return &PipelineTopics{
					pipeline: parts[0],
					topics: []string{
						fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, parts[1], parts[2]),
					},
				}, nil
			} else {
				return nil, fmt.Errorf("Need to specify either %s or %s for a step", InputsSpecifier, OutputsSpecifier)
			}
		} else {
			return nil, fmt.Errorf("Failed to find step with name %s in pipeline %s", parts[1], parts[0])
		}
	case 4:
		if hasStep(parts[1], response) {
			if parts[2] == InputsSpecifier || parts[2] == OutputsSpecifier {
				return &PipelineTopics{
					pipeline: parts[0],
					topics: []string{
						fmt.Sprintf("%s.%s.%s.%s.%s", SeldonPrefix, DefaultNamespace, ModelSpecifier, parts[1], parts[2]),
					},
					tensor: parts[3],
				}, nil
			} else {
				return nil, fmt.Errorf("Need to specify either %s or %s for a step", InputsSpecifier, OutputsSpecifier)
			}
		} else {
			return nil, fmt.Errorf("Failed to find step with name %s in pipeline %s", parts[1], parts[0])
		}
	default:
		return nil, fmt.Errorf("Bad pipeline specifier %s", pipelineSpec)
	}
}

func (kc *KafkaClient) getPipelineStatus(pipelineSpec string) (*scheduler.PipelineStatusResponse, error) {
	parts := strings.Split(pipelineSpec, ".")
	pipeline := parts[0]
	conn, err := kc.schedulerClient.getConnection()
	if err != nil {
		return nil, err
	}
	grpcClient := scheduler.NewSchedulerClient(conn)
	res, err := kc.schedulerClient.getPipelineStatus(grpcClient, &scheduler.PipelineStatusRequest{SubscriberName: "cli", Name: &pipeline})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (kc *KafkaClient) InspectStep(pipelineStep string, offset int64) error {
	status, err := kc.getPipelineStatus(pipelineStep)
	if err != nil {
		return err
	}
	pipelineTopics, err := createPipelineTopics(pipelineStep, status)
	if err != nil {
		return err
	}

	for _, topic := range pipelineTopics.topics {
		err := kc.readTopic(topic, pipelineTopics.tensor, offset)
		if err != nil {
			return err
		}
	}

	// Fast close requires maybe: https://github.com/confluentinc/confluent-kafka-go/pull/757
	//_ = kc.consumer.Close()
	return nil
}

func (kc *KafkaClient) readTopic(topic string, tensor string, offset int64) error {
	fmt.Printf("---\n%s\n", topic)
	err := kc.subscribeAndSetOffset(topic, offset)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	run := true
	var seen int64
	for run {
		select {
		case <-ctx.Done():
			run = false
		default:
			ev := kc.consumer.Poll(1000)
			if ev == nil {
				continue
			}

			switch e := ev.(type) {
			case *kafka.Message:
				seen = seen + 1
				if strings.HasSuffix(topic, OutputsSpecifier) {
					res := &v2_dataplane.ModelInferResponse{}
					err = proto.Unmarshal(e.Value, res)
					if err != nil {
						return err
					}
					err := updateResponseFromRawContents(res)
					if err != nil {
						return err
					}
					if tensor != "" {
						for _, output := range res.Outputs {
							if output.Name == tensor {
								printProto(output)
							}
						}

					} else {
						printProto(res)
					}
				} else {
					req := &v2_dataplane.ModelInferRequest{}
					err = proto.Unmarshal(e.Value, req)
					if err != nil {
						return err
					}
					err := updateRequestFromRawContents(req)
					if err != nil {
						return err
					}
					if tensor != "" {
						for _, input := range req.Inputs {
							if input.Name == tensor {
								printProto(input)
							}
						}

					} else {
						printProto(req)
					}
				}

				if seen >= offset {
					run = false
				}
			case kafka.Error:
				fmt.Printf("Kafka error %s", e.Error())
			default:
				continue
			}
		}
	}

	return nil
}