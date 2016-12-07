package workflow

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/uber-common/bark"

	workflow "code.uber.internal/devexp/minions/.gen/go/minions"
	"code.uber.internal/devexp/minions/common"
)

const (
	transferTaskLockTimeout          = 10 * time.Second
	transferTaskBatchSize            = 10
	transferProcessorMinPollInterval = 10 * time.Millisecond
	transferProcessorMaxPollInterval = 10 * time.Second
)

type (
	transferQueueProcessorImpl struct {
		executionManager ExecutionPersistence
		taskManager      TaskPersistence
		isStarted        int32
		isStopped        int32
		shutdownWG       sync.WaitGroup
		shutdownCh       chan struct{}
		logger           bark.Logger
	}
)

func newTransferQueueProcessor(executionManager ExecutionPersistence,
	taskManager TaskPersistence, logger bark.Logger) transferQueueProcessor {
	return &transferQueueProcessorImpl{
		executionManager: executionManager,
		taskManager:      taskManager,
		shutdownCh:       make(chan struct{}),
		logger:           logger,
	}
}

func (t *transferQueueProcessorImpl) Start() {
	if !atomic.CompareAndSwapInt32(&t.isStarted, 0, 1) {
		return
	}

	t.shutdownWG.Add(1)
	go t.processorPump()

	// t.logger.Info("Transfer queue processor started.")
}

func (t *transferQueueProcessorImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&t.isStopped, 0, 1) {
		return
	}

	if atomic.LoadInt32(&t.isStarted) == 1 {
		close(t.shutdownCh)
	}

	if success := common.AwaitWaitGroup(&t.shutdownWG, time.Minute); !success {
		t.logger.Warn("Transfer queue processor timed out on shutdown.")
	}

	t.logger.Info("Transfer queue processor stopped.")
}

func (t *transferQueueProcessorImpl) processorPump() {
	defer t.shutdownWG.Done()

	pollInterval := transferProcessorMinPollInterval
	pollTimer := time.NewTimer(pollInterval)
	defer pollTimer.Stop()
	for {
		select {
		case <-t.shutdownCh:
			t.logger.Info("Transfer queue processor pump shutting down.")
			return
		case <-pollTimer.C:
			pollInterval = t.processTransferTasks(pollInterval)
			pollTimer = time.NewTimer(pollInterval)
		}
	}
}

func (t *transferQueueProcessorImpl) processTransferTasks(prevPollInterval time.Duration) time.Duration {
	response, err := t.executionManager.GetTransferTasks(&getTransferTasksRequest{
		lockTimeout: transferTaskLockTimeout,
		batchSize:   transferTaskBatchSize,
	})

	if err != nil {
		t.logger.Warnf("Processor unable to retrieve transfer tasks: %v", err)
		return minDuration(2*prevPollInterval, transferProcessorMaxPollInterval)
	}

	tasks := response.tasks
	if len(tasks) == 0 {
		return minDuration(2*prevPollInterval, transferProcessorMaxPollInterval)
	}

	for _, tsk := range tasks {
		var transferTask task
		switch tsk.taskType {
		case taskTypeActivity:
			transferTask = &activityTask{taskList: tsk.taskList, scheduleID: tsk.scheduleID}
		case taskTypeDecision:
			transferTask = &decisionTask{taskList: tsk.taskList, scheduleID: tsk.scheduleID}
		}
		execution := workflow.WorkflowExecution{WorkflowId: common.StringPtr(tsk.workflowID),
			RunId: common.StringPtr(tsk.runID)}

		_, err1 := t.taskManager.CreateTask(&createTaskRequest{
			execution: execution,
			taskList:  tsk.taskList,
			data:      transferTask,
		})

		if err1 == nil {
			//t.logger.Debugf("Processor transfered taskID '%v' to tasklist '%v' using taskID '%v'.",
			//	tsk.taskID, tsk.taskList, createResponse.taskID)
			err2 := t.executionManager.CompleteTransferTask(&completeTransferTaskRequest{
				execution: execution,
				taskID:    tsk.taskID,
				lockToken: tsk.lockToken,
			})

			if err2 != nil {
				t.logger.Warnf("Processor unable to complete transfer task '%v': %v", tsk.taskID, err2)
			}
		} else {
			t.logger.Warnf("Processor failed to create task: %v", err1)
		}
	}

	return transferProcessorMinPollInterval
}

func minDuration(x, y time.Duration) time.Duration {
	if x < y {
		return x
	}

	return y
}
