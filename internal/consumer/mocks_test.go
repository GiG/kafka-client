// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package consumer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Shopify/sarama"
	cluster "github.com/bsm/sarama-cluster"
)

type (
	mockSaramaConsumer struct {
		sync.Mutex
		closed     int64
		offsets    map[int32]int64
		errorC     chan error
		notifyC    chan *cluster.Notification
		partitionC chan cluster.PartitionConsumer
		messages   chan *sarama.ConsumerMessage
	}
	mockPartitionedConsumer struct {
		id          int32
		topic       string
		closed      int64
		beginOffset int64
		msgC        chan *sarama.ConsumerMessage
	}
	mockDLQProducer struct {
		sync.Mutex
		closed int64
		size   int
		keys   map[string]struct{}
	}
)

func newMockPartitionedConsumer(topic string, id int32, beginOffset int64, rcvBufSize int) *mockPartitionedConsumer {
	return &mockPartitionedConsumer{
		id:          id,
		topic:       topic,
		beginOffset: beginOffset,
		msgC:        make(chan *sarama.ConsumerMessage, rcvBufSize),
	}
}

func (m *mockPartitionedConsumer) start() *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		offset := m.beginOffset + 1
		for i := 0; i < 100; i++ {
			m.sendMsg(offset)
			offset++
		}
		wg.Done()
	}()
	return &wg
}

func (m *mockPartitionedConsumer) stop() {
	close(m.msgC)
}

func (m *mockPartitionedConsumer) sendMsg(offset int64) {
	msg := &sarama.ConsumerMessage{
		Topic:     m.topic,
		Partition: m.id,
		Value:     []byte(fmt.Sprintf("msg-%v", offset)),
		Offset:    offset,
		Timestamp: time.Now(),
	}
	m.msgC <- msg
}

func (m *mockPartitionedConsumer) Close() error {
	atomic.StoreInt64(&m.closed, 1)
	return nil
}

func (m *mockPartitionedConsumer) isClosed() bool {
	return atomic.LoadInt64(&m.closed) == 1
}

// Messages returns the read channel for the messages that are returned by
// the broker.
func (m *mockPartitionedConsumer) Messages() <-chan *sarama.ConsumerMessage {
	return m.msgC
}

// HighWaterMarkOffset returns the high water mark offset of the partition,
// i.e. the offset that will be used for the next message that will be produced.
// You can use this to determine how far behind the processing is.
func (m *mockPartitionedConsumer) HighWaterMarkOffset() int64 {
	return 0
}

// Topic returns the consumed topic name
func (m *mockPartitionedConsumer) Topic() string {
	return m.topic
}

// Partition returns the consumed partition
func (m *mockPartitionedConsumer) Partition() int32 {
	return m.id
}

func newMockSaramaConsumer() *mockSaramaConsumer {
	return &mockSaramaConsumer{
		errorC:     make(chan error, 1),
		notifyC:    make(chan *cluster.Notification, 1),
		partitionC: make(chan cluster.PartitionConsumer, 1),
		offsets:    make(map[int32]int64),
		messages:   make(chan *sarama.ConsumerMessage, 1),
	}
}

func (m *mockSaramaConsumer) offset(id int) int64 {
	m.Lock()
	off, ok := m.offsets[int32(id)]
	m.Unlock()
	if !ok {
		return 0
	}
	return off
}

func (m *mockSaramaConsumer) Errors() <-chan error {
	return m.errorC
}

func (m *mockSaramaConsumer) Notifications() <-chan *cluster.Notification {
	return m.notifyC
}

func (m *mockSaramaConsumer) Partitions() <-chan cluster.PartitionConsumer {
	return m.partitionC
}

func (m *mockSaramaConsumer) CommitOffsets() error {
	return nil
}

func (m *mockSaramaConsumer) Messages() <-chan *sarama.ConsumerMessage {
	return m.messages
}

func (m *mockSaramaConsumer) MarkOffset(msg *sarama.ConsumerMessage, metadata string) {
	m.Lock()
	m.offsets[msg.Partition] = msg.Offset
	m.Unlock()
}

func (m *mockSaramaConsumer) MarkPartitionOffset(topic string, partition int32, offset int64, metadata string) {
	m.Lock()
	m.offsets[partition] = offset
	m.Unlock()
}

func (m *mockSaramaConsumer) HighWaterMarks() map[string]map[int32]int64 {
	result := make(map[string]map[int32]int64)
	result["test"] = make(map[int32]int64)
	m.Lock()
	for k, v := range m.offsets {
		result["test"][k] = v
	}
	m.Unlock()
	return result
}

func (m *mockSaramaConsumer) Close() error {
	atomic.AddInt64(&m.closed, 1)
	return nil
}

func (m *mockSaramaConsumer) isClosed() bool {
	return atomic.LoadInt64(&m.closed) == 1
}

func newMockDLQProducer() *mockDLQProducer {
	return &mockDLQProducer{
		keys: make(map[string]struct{}),
	}
}
func (d *mockDLQProducer) SendMessage(msg *sarama.ProducerMessage) (partition int32, offset int64, err error) {
	d.Lock()
	defer d.Unlock()
	key := string(msg.Key.(sarama.StringEncoder))
	if d.size < 5 {
		// for the first few messages throw errors to test backoff/retry
		if _, ok := d.keys[key]; !ok {
			d.keys[key] = struct{}{}
			return 0, 0, fmt.Errorf("intermittent error")
		}
	}
	d.size++
	d.keys[key] = struct{}{}
	return 0, 0, nil
}

func (d *mockDLQProducer) SendMessages(msgs []*sarama.ProducerMessage) error {
	return fmt.Errorf("not supported")
}
func (d *mockDLQProducer) Close() error {
	d.Lock()
	defer d.Unlock()
	atomic.AddInt64(&d.closed, 1)
	return nil
}
func (d *mockDLQProducer) isClosed() bool {
	d.Lock()
	defer d.Unlock()
	return atomic.LoadInt64(&d.closed) == 1
}
func (d *mockDLQProducer) backlog() int {
	d.Lock()
	defer d.Unlock()
	return d.size
}