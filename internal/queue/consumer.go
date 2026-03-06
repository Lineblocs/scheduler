package queue

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Consumer consumes messages from a RabbitMQ queue.
type Consumer interface {
	Consume(queue string, prefetch int) (<-chan amqp.Delivery, error)
}

// RabbitConsumer implements Consumer using an AMQP channel.
type RabbitConsumer struct {
	ch *amqp.Channel
}

// NewRabbitConsumer creates a Consumer backed by an AMQP channel.
func NewRabbitConsumer(ch *amqp.Channel) *RabbitConsumer {
	return &RabbitConsumer{ch: ch}
}

func (c *RabbitConsumer) Consume(queue string, prefetch int) (<-chan amqp.Delivery, error) {
	if err := c.ch.Qos(prefetch, 0, false); err != nil {
		return nil, fmt.Errorf("queue: set QoS: %w", err)
	}

	// Ensure the queue exists
	_, err := c.ch.QueueDeclare(queue, true, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("queue: declare %s: %w", queue, err)
	}

	msgs, err := c.ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("queue: consume %s: %w", queue, err)
	}

	return msgs, nil
}
