// Copyright © Microsoft <wastore@microsoft.com>
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

package parallel

import (
	"context"
	"sync"
	"time"
)

type crawler struct {
	output      chan ErrorableItem
	workerBody  EnumerateOneDirFunc
	parallelism int
	cond        *sync.Cond
	// the following is protected by cond (and must only be accessed when cond.L is held)
	unstartedDirs      chan Directory // protected by cond.L because we use len() on this, and need to hold lock while making len-based decisions
	dirInProgressCount int64
}

type Directory interface{}
type DirectoryEntry interface{}

type CrawlResult struct {
	item DirectoryEntry
	err  error
}

func (r CrawlResult) Item() (interface{}, error) {
	return r.item, r.err
}

// must be safe to be simultaneously called by multiple go-routines, each with a different dir
type EnumerateOneDirFunc func(dir Directory, enqueueDir func(Directory), enqueueOutput func(DirectoryEntry)) error

func Crawl(ctx context.Context, root Directory, worker EnumerateOneDirFunc, parallelism int) <-chan ErrorableItem {
	c := &crawler{
		unstartedDirs: make(chan Directory, 1000),
		output:        make(chan ErrorableItem, 1000),
		workerBody:    worker,
		parallelism:   parallelism,
		cond:          sync.NewCond(&sync.Mutex{}),
	}
	go c.start(ctx, root)
	return c.output
}

func (c *crawler) start(ctx context.Context, root Directory) {
	done := make(chan struct{})
	heartbeat := func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Second):
				c.cond.Broadcast() // prevent things waiting for ever, even after cancellation has happened
			}
		}
	}
	go heartbeat()

	c.unstartedDirs <- root
	c.runWorkersToCompletion(ctx)
	close(c.output)
	close(done)
}

func (c *crawler) runWorkersToCompletion(ctx context.Context) {
	wg := &sync.WaitGroup{}
	for i := 0; i < c.parallelism; i++ {
		wg.Add(1)
		go c.workerLoop(ctx, wg)
	}
	wg.Wait()
}

func (c *crawler) workerLoop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	var err error
	mayHaveMore := true
	for mayHaveMore && ctx.Err() == nil {
		mayHaveMore, err = c.processOneDirectory(ctx)
		if err != nil {
			c.output <- CrawlResult{err: err}
			// output the error, but we don't necessarily stop the enumeration (e.g. it might be one unreadable dir)
		}
	}
}

func (c *crawler) processOneDirectory(ctx context.Context) (bool, error) {
	var toExamine Directory
	stop := false

	// Acquire a directory to work on
	// Note that we need explicit locking because there are two
	// mutable things involved in our decision making, not one (the two being c.dirs and c.dirInProgressCount)
	// and because we use len(c.unstartedDirs) which is not accurate unless len and channel manipulation are protected
	// by the same lock.
	c.cond.L.Lock()
	{
		// wait while there's nothing to do, and another thread might be going to add something
		for len(c.unstartedDirs) == 0 && c.dirInProgressCount > 0 && ctx.Err() == nil {
			c.cond.Wait() // temporarily relinquish the lock (just on this line only) while we wait for a Signal/Broadcast
		}

		// if we have something to do now, grab it. Else we must be all finished with nothing more to do (ever)
		stop = ctx.Err() != nil
		if !stop {
			select {
			case toExamine = <-c.unstartedDirs:
				c.dirInProgressCount++ // record that we are working on something
				c.cond.Broadcast()     // and let other threads know of that fact
			default:
				if c.dirInProgressCount > 0 {
					// something has gone wrong in the design of this algorithm, because we should only get here if all done now
					panic("assertion failure: should be no more dirs in progress here")
				}
				stop = true
			}
		}
	}
	c.cond.L.Unlock()
	if stop {
		return false, nil
	}

	// find dir's immediate children (outside the lock, because this could be slow)
	var foundDirectories = make([]Directory, 0, 16)
	addDir := func(d Directory) {
		foundDirectories = append(foundDirectories, d)
	}
	addOutput := func(e DirectoryEntry) {
		c.output <- CrawlResult{item: e}
	}
	bodyErr := c.workerBody(toExamine, addDir, addOutput) // this is the worker body supplied by our caller

	// finally, update shared state (inside the lock)
	c.cond.L.Lock()
	defer c.cond.L.Unlock()
	for _, d := range foundDirectories {
		c.unstartedDirs <- d
	}
	c.dirInProgressCount-- // we were doing something, and now we have finished it
	c.cond.Broadcast()     // let other workers know that the state has changed

	return true, bodyErr // true because, as far as we know, the work is not finished. And err because it was the err (if any) from THIS dir
}
