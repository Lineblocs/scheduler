package queue

import (
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	"lineblocs.com/scheduler/internal/config"
)

// Connection wraps an AMQP connection.
type Connection struct {
	Conn *amqp.Connection
}

// NewConnection creates a new RabbitMQ connection from config.
func NewConnection(cfg *config.Config) (*Connection, error) {
	conn, err := amqp.Dial(cfg.Rabbit.URL)
	if err != nil {
		return nil, fmt.Errorf("queue: failed to connect to RabbitMQ: %w", err)
	}
	return &Connection{Conn: conn}, nil
}

// Close closes the underlying AMQP connection.
func (c *Connection) Close() error {
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}

// Channel opens a new AMQP channel.
func (c *Connection) Channel() (*amqp.Channel, error) {
	ch, err := c.Conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("queue: failed to open channel: %w", err)
	}
	return ch, nil
}
