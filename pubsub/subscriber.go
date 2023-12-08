package pubsub

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/willdot/messagebroker/server"
)

type connOpp func(conn net.Conn) error

// Subscriber allows subscriptions to a server and the consumption of messages
type Subscriber struct {
	conn   net.Conn
	connMu sync.Mutex
}

// NewSubscriber will connect to the server at the given address
func NewSubscriber(addr string) (*Subscriber, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial: %w", err)
	}

	return &Subscriber{
		conn: conn,
	}, nil
}

// Close cleanly shuts down the subscriber
func (s *Subscriber) Close() error {
	return s.conn.Close()
}

// SubscribeToTopics will subscribe to the provided topics
func (s *Subscriber) SubscribeToTopics(topicNames []string) error {
	op := func(conn net.Conn) error {
		err := binary.Write(conn, binary.BigEndian, server.Subscribe)
		if err != nil {
			return fmt.Errorf("failed to subscribe: %w", err)
		}

		b, err := json.Marshal(topicNames)
		if err != nil {
			return fmt.Errorf("failed to marshal topic names: %w", err)
		}

		err = binary.Write(conn, binary.BigEndian, uint32(len(b)))
		if err != nil {
			return fmt.Errorf("failed to write topic data length: %w", err)
		}

		_, err = conn.Write(b)
		if err != nil {
			return fmt.Errorf("failed to subscribe to topics: %w", err)
		}

		var resp server.Status
		err = binary.Read(conn, binary.BigEndian, &resp)
		if err != nil {
			return fmt.Errorf("failed to read confirmation of subscription: %w", err)
		}

		if resp == server.Subscribed {
			return nil
		}

		var dataLen uint32
		err = binary.Read(conn, binary.BigEndian, &dataLen)
		if err != nil {
			return fmt.Errorf("received status %s:", resp)
		}

		buf := make([]byte, dataLen)
		_, err = conn.Read(buf)
		if err != nil {
			return fmt.Errorf("received status %s:", resp)
		}

		return fmt.Errorf("received status %s - %s", resp, buf)
	}

	return s.connOperation(op)
}

// UnsubscribeToTopics will unsubscribe to the provided topics
func (s *Subscriber) UnsubscribeToTopics(topicNames []string) error {
	op := func(conn net.Conn) error {
		err := binary.Write(conn, binary.BigEndian, server.Unsubscribe)
		if err != nil {
			return fmt.Errorf("failed to unsubscribe: %w", err)
		}

		b, err := json.Marshal(topicNames)
		if err != nil {
			return fmt.Errorf("failed to marshal topic names: %w", err)
		}

		err = binary.Write(conn, binary.BigEndian, uint32(len(b)))
		if err != nil {
			return fmt.Errorf("failed to write topic data length: %w", err)
		}

		_, err = conn.Write(b)
		if err != nil {
			return fmt.Errorf("failed to unsubscribe to topics: %w", err)
		}

		var resp server.Status
		err = binary.Read(conn, binary.BigEndian, &resp)
		if err != nil {
			return fmt.Errorf("failed to read confirmation of unsubscription: %w", err)
		}

		if resp == server.Unsubscribed {
			return nil
		}

		var dataLen uint32
		err = binary.Read(conn, binary.BigEndian, &dataLen)
		if err != nil {
			return fmt.Errorf("received status %s:", resp)
		}

		buf := make([]byte, dataLen)
		_, err = conn.Read(buf)
		if err != nil {
			return fmt.Errorf("received status %s:", resp)
		}

		return fmt.Errorf("received status %s - %s", resp, buf)
	}

	return s.connOperation(op)
}

// Consumer allows the consumption of messages. If during the consumer receiving messages from the
// server an error occurs, it will be stored in Err
type Consumer struct {
	msgs chan Message
	// TODO: better error handling? Maybe a channel of errors?
	Err error
}

// Messages returns a channel in which this consumer will put messages onto. It is safe to range over the channel since it will be closed once
// the consumer has finished either due to an error or from being cancelled.
func (c *Consumer) Messages() <-chan Message {
	return c.msgs
}

// Consume will create a consumer and start it running in a go routine. You can then use the Msgs channel of the consumer
// to read the messages
func (s *Subscriber) Consume(ctx context.Context) *Consumer {
	consumer := &Consumer{
		msgs: make(chan Message),
	}

	go s.consume(ctx, consumer)

	return consumer
}

func (s *Subscriber) consume(ctx context.Context, consumer *Consumer) {
	defer close(consumer.msgs)
	for {
		if ctx.Err() != nil {
			return
		}

		msg, err := s.readMessage()
		if err != nil {
			consumer.Err = err
			return
		}

		if msg != nil {
			consumer.msgs <- *msg
		}
	}
}

func (s *Subscriber) readMessage() (*Message, error) {
	var msg *Message
	op := func(conn net.Conn) error {
		err := s.conn.SetReadDeadline(time.Now().Add(time.Second))
		if err != nil {
			return err
		}

		var topicLen uint64
		err = binary.Read(s.conn, binary.BigEndian, &topicLen)
		if err != nil {
			// TODO: check if this is needed elsewhere. I'm not sure where the read deadline resets....
			if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
				return nil
			}
			return err
		}

		topicBuf := make([]byte, topicLen)
		_, err = s.conn.Read(topicBuf)
		if err != nil {
			return err
		}

		var dataLen uint64
		err = binary.Read(s.conn, binary.BigEndian, &dataLen)
		if err != nil {
			return err
		}

		if dataLen <= 0 {
			return nil
		}

		dataBuf := make([]byte, dataLen)
		_, err = s.conn.Read(dataBuf)
		if err != nil {
			return err
		}

		msg = &Message{
			Data:  dataBuf,
			Topic: string(topicBuf),
		}

		return nil

	}

	err := s.connOperation(op)
	if err != nil {
		var neterr net.Error
		if errors.As(err, &neterr) && neterr.Timeout() {
			return nil, nil
		}
		return nil, err
	}

	return msg, err
}

func (s *Subscriber) connOperation(op connOpp) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	return op(s.conn)
}