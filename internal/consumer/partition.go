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
	"time"

	"math"
	"strconv"

	"github.com/Shopify/sarama"
	cluster "github.com/bsm/sarama-cluster"
	"github.com/uber-go/kafka-client/internal/list"
	"github.com/uber-go/kafka-client/internal/metrics"
	"github.com/uber-go/kafka-client/internal/util"
	"github.com/uber-go/kafka-client/kafka"
	"github.com/uber-go/tally"
	"go.uber.org/zap"
)

type (
	// partitionConsumer is the consumer for a specific
	// kafka partition
	partitionConsumer struct {
		id        int32
		topic     string
		msgCh     chan kafka.Message
		ackMgr    *ackManager
		sarama    SaramaConsumer
		pConsumer cluster.PartitionConsumer
		dlq       DLQ
		options   *Options
		tally     tally.Scope
		logger    *zap.Logger
		stopC     chan struct{}
		lifecycle *util.RunLifecycle
	}
)

// newPartitionConsumer returns a kafka consumer that can
// read messages from a given [ topic, partition ] tuple
func newPartitionConsumer(
	sarama SaramaConsumer,
	pConsumer cluster.PartitionConsumer,
	options *Options,
	msgCh chan kafka.Message,
	dlq DLQ,
	scope tally.Scope,
	logger *zap.Logger) *partitionConsumer {
	maxUnAcked := options.Concurrency + options.RcvBufferSize + 1
	name := fmt.Sprintf("%v-partition-%v", pConsumer.Topic(), pConsumer.Partition())
	return &partitionConsumer{
		id:        pConsumer.Partition(),
		topic:     pConsumer.Topic(),
		sarama:    sarama,
		pConsumer: pConsumer,
		options:   options,
		msgCh:     msgCh,
		dlq:       dlq,
		tally:     scope.Tagged(map[string]string{"partition": strconv.Itoa(int(pConsumer.Partition()))}),
		logger:    logger,
		stopC:     make(chan struct{}),
		ackMgr:    newAckManager(maxUnAcked, scope, logger),
		lifecycle: util.NewRunLifecycle(name, logger),
	}
}

// Start starts the consumer
func (p *partitionConsumer) Start() error {
	return p.lifecycle.Start(func() error {
		go p.messageLoop()
		go p.commitLoop()
		p.tally.Counter(metrics.KafkaPartitionStarted).Inc(1)
		return nil
	})
}

// Stop stops the consumer immediately
// Does not wait for inflight messages
// to be drained
func (p *partitionConsumer) Stop() {
	p.stop(time.Duration(0))
}

// Drain gracefully stops this consumer
// - Waits for inflight messages to be drained
// - Checkpoints the latest offset to the broker
// - Stops the underlying consumer
func (p *partitionConsumer) Drain(d time.Duration) {
	p.stop(d)
}

// messageLoop is the message read loop for this consumer
// todo: maintain a pre-allocated pool of Messages
func (p *partitionConsumer) messageLoop() {
	p.logInfo("partition consumer started")
	for {
		select {
		case m, ok := <-p.pConsumer.Messages():
			if !ok {
				p.logInfo("partition message channel closed")
				p.Drain(p.options.MaxProcessingTime)
				return
			}
			lag := time.Now().Sub(m.Timestamp)
			p.tally.Gauge(metrics.KafkaPartitionLag).Update(float64(lag))
			p.tally.Gauge(metrics.KafkaPartitionReadOffset).Update(float64(m.Offset))
			p.tally.Counter(metrics.KafkaPartitionMessagesIn).Inc(1)
			p.deliver(m)
		case <-p.stopC:
			p.logInfo("partition consumer stopped")
			return
		}
	}
}

// commitLoop periodically checkpoints the offsets with broker
func (p *partitionConsumer) commitLoop() {
	ticker := time.NewTicker(p.options.MaxProcessingTime)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.markOffset()
		case <-p.stopC:
			p.markOffset()
			return
		}
	}
}

// markOffset checkpoints the latest offset
func (p *partitionConsumer) markOffset() {
	latestOff := p.ackMgr.CommitLevel()
	if latestOff >= 0 {
		p.sarama.MarkPartitionOffset(p.topic, p.id, latestOff, "")
		p.tally.Gauge(metrics.KafkaPartitionCommitOffset).Update(float64(latestOff))
		backlog := math.Max(float64(0), float64(p.pConsumer.HighWaterMarkOffset()-latestOff))
		p.tally.Gauge(metrics.KafkaPartitionBacklog).Update(backlog)
		p.logger.Debug("kafka checkpoint",
			zap.String("topic", p.topic), zap.Int32("partition", p.id), zap.Int64("offset", latestOff))
	}
}

// deliver delivers a message through the consumer channel
func (p *partitionConsumer) deliver(scm *sarama.ConsumerMessage) {
	ackID, err := p.trackOffset(scm.Offset)
	if err != nil {
		return
	}
	msg := newMessage(scm, ackID, p.ackMgr, p.dlq)
	select {
	case p.msgCh <- msg:
		return
	case <-p.stopC:
		return
	}
}

// trackOffset adds the given offset to the list of unacked
func (p *partitionConsumer) trackOffset(offset int64) (ackID, error) {
	for {
		id, err := p.ackMgr.GetAckID(offset)
		if err != nil {
			if err != list.ErrCapacity {
				return ackID{}, err
			}
			p.tally.Counter(metrics.KafkaPartitionAckMgrListFull).Inc(1)
			p.logger.Error("ackMgr list ran out of capacity",
				zap.String("topic", p.topic),
				zap.Int32("partition", p.id))
			// list can never be out of capacity if maxOutstanding
			// is configured correctly for ackMgr, but if not, we have no
			// option but to spin in this case
			if p.sleep(time.Millisecond * 100) {
				return ackID{}, fmt.Errorf("consumer stopped")
			}
			continue
		}
		return id, nil
	}
}

func (p *partitionConsumer) stop(d time.Duration) {
	p.lifecycle.Stop(func() {
		close(p.stopC)
		time.Sleep(d)
		p.markOffset()
		p.pConsumer.Close()
		p.tally.Counter(metrics.KafkaPartitionStopped).Inc(1)
	})
}

func (p *partitionConsumer) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return false
	case <-p.stopC:
		return true
	}
}

func (p *partitionConsumer) logInfo(msg string) {
	p.logger.Info(msg, zap.String("topic", p.topic), zap.Int32("partition", p.id))
}