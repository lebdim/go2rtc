package streams

import (
	"sync"
	"testing"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/stretchr/testify/require"
)

type fakeProducer struct{ core.Connection }

func (p *fakeProducer) GetMedias() []*core.Media { return nil }
func (p *fakeProducer) GetTrack(*core.Media, *core.Codec) (*core.Receiver, error) {
	return nil, nil
}
func (p *fakeProducer) Start() error { return nil }
func (p *fakeProducer) Stop() error  { return nil }

func TestActiveProducersReturnsOnlyActiveConnections(t *testing.T) {
	conn1 := &fakeProducer{}
	conn2 := &fakeProducer{}

	stream := &Stream{
		producers: []*Producer{
			{conn: conn1},
			{},
			{conn: conn2},
		},
	}

	active := stream.ActiveProducers()
	require.Len(t, active, 2)
	require.Same(t, conn1, active[0])
	require.Same(t, conn2, active[1])
}

func TestActiveProducersUsesProducerLock(t *testing.T) {
	stream := &Stream{
		producers: []*Producer{{}},
	}

	conn1 := &fakeProducer{}
	conn2 := &fakeProducer{}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			stream.producers[0].mu.Lock()
			switch i % 3 {
			case 0:
				stream.producers[0].conn = conn1
			case 1:
				stream.producers[0].conn = conn2
			default:
				stream.producers[0].conn = nil
			}
			stream.producers[0].mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			active := stream.ActiveProducers()
			if len(active) == 0 {
				continue
			}
			require.True(t, active[0] == conn1 || active[0] == conn2)
		}
	}()

	wg.Wait()
}
