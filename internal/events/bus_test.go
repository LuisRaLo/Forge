package events

import (
	"sync"
	"testing"
	"time"
)

func TestPublishReachesSubscriber(t *testing.T) {
	t.Parallel()
	var b Bus
	ch, unsubscribe := b.Subscribe(4)
	defer unsubscribe()

	b.Publish(Event{Type: TypeTaskCreated, TaskID: "TASK-1"})

	select {
	case ev := <-ch:
		if ev.TaskID != "TASK-1" {
			t.Errorf("unexpected event: %+v", ev)
		}
		if ev.Timestamp.IsZero() {
			t.Error("Publish must stamp a timestamp when none is given")
		}
	case <-time.After(time.Second):
		t.Fatal("event never arrived")
	}
}

func TestPublishFansOutToEverySubscriber(t *testing.T) {
	t.Parallel()
	var b Bus
	ch1, unsub1 := b.Subscribe(4)
	defer unsub1()
	ch2, unsub2 := b.Subscribe(4)
	defer unsub2()

	b.Publish(Event{TaskID: "TASK-1"})

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("event did not reach a subscriber")
		}
	}
}

func TestPublishWithNoSubscribersDoesNotBlock(t *testing.T) {
	t.Parallel()
	var b Bus
	done := make(chan struct{})
	go func() {
		b.Publish(Event{TaskID: "TASK-1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked with no subscribers")
	}
}

func TestPublishDropsRatherThanBlocksOnFullBuffer(t *testing.T) {
	t.Parallel()
	var b Bus
	ch, unsubscribe := b.Subscribe(1)
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(Event{TaskID: "TASK-1"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer — a slow client must never stall the publisher")
	}
	<-ch // drain the one buffered event, proving the channel is otherwise usable
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()
	var b Bus
	ch, unsubscribe := b.Subscribe(4)
	unsubscribe()

	_, ok := <-ch
	if ok {
		t.Fatal("expected the channel to be closed after unsubscribe")
	}
}

func TestUnsubscribeStopsFurtherDelivery(t *testing.T) {
	t.Parallel()
	var b Bus
	ch, unsubscribe := b.Subscribe(4)
	unsubscribe()

	// Publishing after unsubscribe must not panic (send on closed channel).
	b.Publish(Event{TaskID: "TASK-1"})
	if _, ok := <-ch; ok {
		t.Fatal("expected no further delivery after unsubscribe")
	}
}

func TestConcurrentSubscribeAndPublish(t *testing.T) {
	t.Parallel()
	var b Bus
	var wg sync.WaitGroup

	wg.Add(20)
	for i := 0; i < 10; i++ {
		go func() {
			defer wg.Done()
			ch, unsubscribe := b.Subscribe(8)
			defer unsubscribe()
			select {
			case <-ch:
			case <-time.After(200 * time.Millisecond):
			}
		}()
		go func() {
			defer wg.Done()
			b.Publish(Event{TaskID: "TASK-1"})
		}()
	}
	wg.Wait()
}
