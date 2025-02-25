/*
Copyright 2018 The Kubernetes Authors.
Copyright 2023 The OpenYurt Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// Controller implements controller.Controller.
type Controller[request comparable] struct {
	// Name is used to uniquely identify a Controller in tracing, logging and monitoring.  Name is required.
	Name string

	// RateLimiter is used to limit how frequently requests may be queued into the work queue.
	RateLimiter workqueue.TypedRateLimiter[request]

	// MakeQueue constructs the queue for this controller once the controller is ready to start.
	// This exists because the standard Kubernetes workqueues start themselves immediately, which
	// leads to goroutine leaks if something calls controller.New repeatedly.
	NewQueue func(controllerName string, rateLimiter workqueue.TypedRateLimiter[request]) workqueue.TypedRateLimitingInterface[request]

	// Queue is an listeningQueue that listens for events from Informers and adds object keys to
	// the Queue for processing
	Queue workqueue.TypedRateLimitingInterface[request]

	// mu is used to synchronize Controller setup
	mu sync.Mutex

	// Started is true if the Controller has been Started
	Started bool

	// ctx is the context that was passed to Start() and used when starting watches.
	//
	// According to the docs, contexts should not be stored in a struct: https://golang.org/pkg/context,
	// while we usually always strive to follow best practices, we consider this a legacy case and it should
	// undergo a major refactoring and redesign to allow for context to not be stored in a struct.
	ctx context.Context

	// CacheSyncTimeout refers to the time limit set on waiting for cache to sync
	// Defaults to 2 minutes if not set.
	CacheSyncTimeout time.Duration

	// startWatches maintains a list of sources, handlers, and predicates to start when the controller is started.
	startWatches []source.TypedSource[request]

	// LogConstructor is used to construct a logger to then log messages to users during reconciliation,
	// or for example when a watch is started.
	// Note: LogConstructor has to be able to handle nil requests as we are also using it
	// outside the context of a reconciliation.
	LogConstructor func(request *request) logr.Logger

	// RecoverPanic indicates whether the panic caused by reconcile should be recovered.
	RecoverPanic *bool

	// LeaderElected indicates whether the controller is leader elected or always running.
	LeaderElected *bool
}

// Watch implements controller.Controller.
func (c *Controller[request]) Watch(src source.TypedSource[request]) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Controller hasn't started yet, store the watches locally and return.
	//
	// These watches are going to be held on the controller struct until the manager or user calls Start(...).
	if !c.Started {
		c.startWatches = append(c.startWatches, src)
		return nil
	}

	c.LogConstructor(nil).Info("Starting EventSource", "source", src)
	return src.Start(c.ctx, c.Queue)
}

// NeedLeaderElection implements the manager.LeaderElectionRunnable interface.
func (c *Controller[request]) NeedLeaderElection() bool {
	if c.LeaderElected == nil {
		return true
	}
	return *c.LeaderElected
}

// Start implements controller.Controller.
func (c *Controller[request]) Start(ctx context.Context) error {
	// use an IIFE to get proper lock handling
	// but lock outside to get proper handling of the queue shutdown
	c.mu.Lock()
	if c.Started {
		return errors.New("controller was started more than once. This is likely to be caused by being added to a manager multiple times")
	}

	// Set the internal context.
	c.ctx = ctx

	c.Queue = c.NewQueue(c.Name, c.RateLimiter)
	go func() {
		<-ctx.Done()
		c.Queue.ShutDown()
	}()

	err := func() error {
		defer c.mu.Unlock()

		// TODO(pwittrock): Reconsider HandleCrash
		defer utilruntime.HandleCrash()

		// NB(directxman12): launch the sources *before* trying to wait for the
		// caches to sync so that they have a chance to register their intendeded
		// caches.
		for _, watch := range c.startWatches {
			c.LogConstructor(nil).Info("Starting EventSource", "source", fmt.Sprintf("%s", watch))

			if err := watch.Start(ctx, c.Queue); err != nil {
				return err
			}
		}

		// Start the SharedIndexInformer factories to begin populating the SharedIndexInformer caches
		c.LogConstructor(nil).Info("Starting Controller")

		for _, watch := range c.startWatches {
			syncingSource, ok := watch.(source.TypedSyncingSource[request])
			if !ok {
				continue
			}

			if err := func() error {
				// use a context with timeout for launching sources and syncing caches.
				sourceStartCtx, cancel := context.WithTimeout(ctx, c.CacheSyncTimeout)
				defer cancel()

				// WaitForSync waits for a definitive timeout, and returns if there
				// is an error or a timeout
				if err := syncingSource.WaitForSync(sourceStartCtx); err != nil {
					err := fmt.Errorf("could not wait for %s caches to sync: %w", c.Name, err)
					klog.ErrorS(err, "Could not wait for Cache to sync")
					return err
				}

				return nil
			}(); err != nil {
				return err
			}
		}

		// All the watches have been started, we can reset the local slice.
		//
		// We should never hold watches more than necessary, each watch source can hold a backing cache,
		// which won't be garbage collected if we hold a reference to it.
		c.startWatches = nil

		klog.V(2).InfoS("No workers need to be started", "controller", c.Name)
		c.Started = true
		return nil
	}()
	if err != nil {
		return err
	}

	<-ctx.Done()
	klog.V(2).InfoS("No Reconcile controller finished", "controller", c.Name)
	return nil
}

func (c *Controller[request]) WaitForStarted(ctx context.Context) bool {
	err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		c.mu.Lock()
		started := c.Started
		c.mu.Unlock()
		if !started {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		klog.V(2).InfoS("could not start %s controller , %v", c.Name, err)
		return false
	}

	klog.V(2).InfoS("%s controller started", c.Name)
	return true
}
