package queue

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Publisher publishes messages to RabbitMQ queues.
type Publisher interface {
	Publish(queue string, message []byte) error
	PublishWithContext(ctx context.Context, queue string, message []byte) error
}

// RabbitPublisher implements Publisher using an AMQP channel.
type RabbitPublisher struct {
	ch *amqp.Channel
}

// NewRabbitPublisher creates a Publisher backed by an AMQP channel.
func NewRabbitPublisher(ch *amqp.Channel) *RabbitPublisher {
	return &RabbitPublisher{ch: ch}
}

func (p *RabbitPublisher) Publish(queue string, message []byte) error {
	return p.PublishWithContext(context.Background(), queue, message)
}

func (p *RabbitPublisher) PublishWithContext(ctx context.Context, queue string, message []byte) error {
	err := p.ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		Body:         message,
	})
	if err != nil {
		return fmt.Errorf("queue: publish to %s: %w", queue, err)
	}
	return nil
}
