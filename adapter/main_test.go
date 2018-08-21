package main

import (
	"testing"
	"os"
	"github.com/prometheus/common/model"
	"sync"
	"time"
	"log"
	"github.com/meson10/highbrow"
)

var limit = 10

type MemoryStore struct {
	sync.Mutex
	store []*model.Samples
}

func (i *MemoryStore) Send(p *model.Samples) error {
	i.Lock()
	defer i.Unlock()

	log.Println(p)

	i.store = append(i.store, p)
	return nil
}

func TestChannel(t *testing.T) {
	writer := MemoryStore{}
	ch := make(chan *model.Samples)

	l := makeLimiter()

	l.SetRate(40).SetBurst(10)
	go read(l, &writer, ch)

	var wg sync.WaitGroup
	wg.Add(1)
	writeSamplesToChannel(ch)
	wg.Done()

	t.Run("Should have read x number of messages per second", func(t *testing.T) {
		wg.Wait()

		expected := limit * 3
		actual := len(writer.store)
		if actual < expected {
			t.Fatalf("Should have processed %v. Got %v", expected, actual)
		}
		if actual != expected {
			t.Fatalf("The count of messages expected to be %v, got %v", expected, actual)
		}
	})

	t.Run("Number of messages read should increase with time.", func(t *testing.T) {
		wg.Wait()
		beforeSleep := len(writer.store)
		writeSamplesToChannel(ch)
		time.Sleep(5 * time.Second)
		afterSleep := len(writer.store)
		if afterSleep <= beforeSleep {
			t.Fatalf("The count of messages should have increased.")
		}
	})
}


func TestMain(m *testing.M) {
	os.Exit(m.Run())
}


func writeSamplesToChannel(ch chan<- *model.Samples) {
	var s *model.Samples
	for _, t := range []string{"t1", "t2", "t3"} {
		for i := 0; i < limit; i++ {
			s = &model.Samples{{
				Metric: model.Metric{
					model.MetricNameLabel: model.LabelValue(t),
				},
				Timestamp: model.Time(time.Now().Unix()),
				Value:     model.SampleValue(i),
			}}

			ch <- s
		}
	}
}

func read(l *highbrow.RateLimiter, writer *MemoryStore, mChannel chan *model.Samples) {
	go handleInterrupts()

	t := l.Start()
	for {
		<-t
		y := <-mChannel
		go writer.Send(y)
	}

	l.Stop()
}
