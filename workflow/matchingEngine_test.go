package workflow

import (
	"errors"
	"os"
	"testing"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-common/bark"

	workflow "code.uber.internal/devexp/minions/.gen/go/shared"
	"code.uber.internal/devexp/minions/common"
	"code.uber.internal/devexp/minions/persistence"
	"code.uber.internal/devexp/minions/persistence/mocks"
)

type (
	matchingEngineSuite struct {
		suite.Suite
		TestBase
		builder            *historyBuilder
		mockMatchingEngine *matchingEngineImpl
		mockTaskMgr        *mocks.TaskManager
		mockExecutionMgr   *mocks.ExecutionManager
		logger             bark.Logger
	}
)

func TestMatchingEngineSuite(t *testing.T) {
	s := new(matchingEngineSuite)
	suite.Run(t, s)
}

func (s *matchingEngineSuite) SetupSuite() {
	if testing.Verbose() {
		log.SetOutput(os.Stdout)
	}

	s.SetupWorkflowStore()

	s.logger = bark.NewLoggerFromLogrus(log.New())
	s.builder = newHistoryBuilder(nil, s.logger)
}

func (s *matchingEngineSuite) TearDownSuite() {
	s.TearDownWorkflowStore()
}

func (s *matchingEngineSuite) SetupTest() {
	s.mockTaskMgr = &mocks.TaskManager{}
	s.mockExecutionMgr = &mocks.ExecutionManager{}

	mockShard := &shardContextImpl{
		shardInfo:              &persistence.ShardInfo{ShardID: 1, RangeID: 1, TransferAckLevel: 0},
		transferSequenceNumber: 1,
	}

	history := &historyEngineImpl{
		shard:            mockShard,
		executionManager: s.mockExecutionMgr,
		txProcessor:      newTransferQueueProcessor(mockShard, s.mockExecutionMgr, s.mockTaskMgr, s.logger),
		logger:           s.logger,
		tokenSerializer:  newJSONTaskTokenSerializer(),
	}
	history.timerProcessor = newTimerQueueProcessor(history, s.mockExecutionMgr, s.logger)

	s.mockMatchingEngine = &matchingEngineImpl{
		taskManager:     s.mockTaskMgr,
		historyService:  history,
		logger:          s.logger,
		tokenSerializer: newJSONTaskTokenSerializer(),
	}
}

func (s *matchingEngineSuite) TearDownTest() {
	s.mockTaskMgr.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())
}

func (s *matchingEngineSuite) TestPollForActivityTasks() {
	tl := "makeToast"
	identity := "selfDrivingToaster"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{TaskList: tl, TaskType: persistence.TaskTypeActivity, LockTimeout: taskLockDuration, BatchSize: 1}

	// There are no tasks
	s.mockTaskMgr.On("GetTasks", taskRequest).Return(&persistence.GetTasksResponse{}, nil).Once()
	_, err := s.mockMatchingEngine.pollForActivityTaskOperation(&workflow.PollForActivityTaskRequest{
		TaskList: taskList,
		Identity: &identity})
	s.Equal(errNoTasks, err)
	s.mockTaskMgr.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())

	// Can't get tasks
	s.mockTaskMgr.On("GetTasks", taskRequest).Return(nil, errors.New("Out of bread")).Once()
	_, err = s.mockMatchingEngine.PollForActivityTask(&workflow.PollForActivityTaskRequest{
		TaskList: taskList,
		Identity: &identity})
	s.EqualError(err, "Out of bread")
	s.mockTaskMgr.AssertExpectations(s.T())
	s.mockExecutionMgr.AssertExpectations(s.T())

	// Lets do something
	resp := &persistence.GetTasksResponse{Tasks: []*persistence.TaskInfoWithID{{
		"tId",
		&persistence.TaskInfo{
			WorkflowID:     "wId",
			RunID:          "rId",
			TaskID:         int64(1),
			TaskList:       tl,
			TaskType:       1,
			ScheduleID:     1 + firstEventID,
			VisibilityTime: time.Time{},
			LockToken:      "lock",
			DeliveryCount:  0,
		}}}}
	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	scheduledEvent := builder.AddDecisionTaskScheduledEvent(tl, 2)
	actType := workflow.NewActivityType()
	actType.Name = common.StringPtr("Dynamic type")

	builder.AddActivityTaskScheduledEvent(*scheduledEvent.EventId,
		&workflow.ScheduleActivityTaskDecisionAttributes{
			ActivityId:   common.StringPtr("Very unique id"),
			ActivityType: actType,
			Input:        []byte{9, 8, 7},
		})

	history, err := builder.Serialize()
	s.Nil(err)

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(resp, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(
		&persistence.GetWorkflowExecutionResponse{
			ExecutionInfo: &persistence.WorkflowExecutionInfo{
				WorkflowID:           "wId",
				RunID:                "rId",
				TaskList:             "tId",
				History:              history,
				ExecutionContext:     nil,
				State:                3,
				NextEventID:          1,
				LastProcessedEvent:   0,
				LastUpdatedTimestamp: time.Time{},
				DecisionPending:      false},
		},
		nil).Once()

	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()
	task, err := s.mockMatchingEngine.PollForActivityTask(&workflow.PollForActivityTaskRequest{
		TaskList: taskList,
		Identity: &identity})
	s.Nil(err)
	s.Equal("Very unique id", task.GetActivityId())
	s.Equal([]byte{9, 8, 7}, task.GetInput())
	s.Equal(actType, task.GetActivityType())

	var updateRequest *persistence.UpdateWorkflowExecutionRequest
	ok := false
	if updateRequest, ok = s.mockExecutionMgr.Calls[len(s.mockExecutionMgr.Calls)-1].Arguments[0].(*persistence.UpdateWorkflowExecutionRequest); !ok {
		s.Fail("Wrong parameter type was passed to UpdateWorkflowExecution")
	}

	// Examine history
	updatedHistory, err := newJSONHistorySerializer().Deserialize(updateRequest.ExecutionInfo.History)
	s.Nil(err)

	// We should have 3 events: Decision, ScheduledTask and StartedTask
	s.Equal(3, len(updatedHistory))
	s.Equal(0, len(updateRequest.TransferTasks))

}

func (s *matchingEngineSuite) TestPollForActivityTasksIfTaskAlreadyStarted() {
	id := "TestPollForActivityTasksIfTaskAlreadyStarted"
	wt := "UT"
	tl := "makeBreakfast"
	identity := "mickey mouse"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl
	workflowType := workflow.NewWorkflowType()
	workflowType.Name = common.StringPtr(wt)

	taskRequest := &persistence.GetTasksRequest{TaskList: "makeBreakfast", TaskType: 1, LockTimeout: taskLockDuration, BatchSize: 1}

	resp := &persistence.GetTasksResponse{Tasks: []*persistence.TaskInfoWithID{{
		"tId",
		&persistence.TaskInfo{
			WorkflowID:     "wId",
			RunID:          "rId",
			TaskID:         int64(1),
			TaskList:       tl,
			TaskType:       1,
			ScheduleID:     2 + firstEventID,
			VisibilityTime: time.Time{},
			LockToken:      "lock",
			DeliveryCount:  0,
		}}}}
	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))

	request := &workflow.StartWorkflowExecutionRequest{
		WorkflowId:   common.StringPtr(id),
		WorkflowType: workflowType,
		TaskList:     taskList,
		Input:        nil,
		ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(100),
		TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
		Identity:                            common.StringPtr(identity),
	}

	builder.AddWorkflowExecutionStartedEvent(request)
	scheduledEvent := builder.AddDecisionTaskScheduledEvent(tl, 2)

	taskScheduled := builder.AddActivityTaskScheduledEvent(*scheduledEvent.EventId, &workflow.ScheduleActivityTaskDecisionAttributes{})
	started := builder.AddActivityTaskStartedEvent(*taskScheduled.EventId, &workflow.PollForActivityTaskRequest{TaskList: taskList, Identity: &identity})
	history, err := builder.Serialize()
	s.Nil(err)

	// Fail GetWorkflowExecution first time and pass on the retry.
	s.mockTaskMgr.On("GetTasks", taskRequest).Return(resp, nil).Twice()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Twice()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, errors.New("Let's get dangerous!")).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(
		&persistence.GetWorkflowExecutionResponse{
			ExecutionInfo: &persistence.WorkflowExecutionInfo{
				WorkflowID:           "wId",
				RunID:                "rId",
				TaskList:             "tId",
				History:              history,
				ExecutionContext:     nil,
				State:                3,
				NextEventID:          builder.nextEventID,
				LastProcessedEvent:   *started.EventId,
				LastUpdatedTimestamp: time.Time{},
				DecisionPending:      false},
		},
		nil).Once()

	activityTasks, err := s.mockMatchingEngine.pollForActivityTaskOperation(&workflow.PollForActivityTaskRequest{
		TaskList: taskList,
		Identity: &identity})
	s.EqualError(err, "Let's get dangerous!")

	activityTasks, err = s.mockMatchingEngine.pollForActivityTaskOperation(&workflow.PollForActivityTaskRequest{
		TaskList: taskList,
		Identity: &identity})
	s.Equal(err, errDuplicate)

	s.Equal((*workflow.PollForActivityTaskResponse)(nil), activityTasks)
}

func (s *matchingEngineSuite) TestPollForActivityTasksOnConditionalUpdateFail() {
	tl := "drawMickey"
	identity := "Disney"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{TaskList: tl, TaskType: 1, LockTimeout: taskLockDuration, BatchSize: 1}

	resp := &persistence.GetTasksResponse{Tasks: []*persistence.TaskInfoWithID{{
		"tId",
		&persistence.TaskInfo{
			WorkflowID:     "wId",
			RunID:          "rId",
			TaskID:         int64(1),
			TaskList:       tl,
			TaskType:       1,
			ScheduleID:     1 + firstEventID,
			VisibilityTime: time.Time{},
			LockToken:      "lock",
			DeliveryCount:  0,
		}}}}
	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	scheduledEvent := builder.AddDecisionTaskScheduledEvent(tl, 2)

	activity := workflow.NewScheduleActivityTaskDecisionAttributes()
	activity.ActivityType = workflow.NewActivityType()
	activity.ActivityType.Name = common.StringPtr("draw head")
	activity.Input = []byte{1, 2, 3}

	s.NotNil(builder.AddActivityTaskScheduledEvent(*scheduledEvent.EventId, activity))
	history, err := builder.Serialize()
	s.Nil(err)

	// Get tasks and wf execution.
	s.mockTaskMgr.On("GetTasks", taskRequest).Return(resp, nil).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(
		&persistence.GetWorkflowExecutionResponse{
			ExecutionInfo: &persistence.WorkflowExecutionInfo{
				WorkflowID:           "wId",
				RunID:                "rId",
				TaskList:             "tId",
				History:              history,
				ExecutionContext:     nil,
				State:                3,
				NextEventID:          1,
				LastProcessedEvent:   0,
				LastUpdatedTimestamp: time.Time{},
				DecisionPending:      false},
		},
		nil).Once()

	// Someone added a new activity

	s.NotNil(builder.AddActivityTaskScheduledEvent(*scheduledEvent.EventId, workflow.NewScheduleActivityTaskDecisionAttributes()))
	history, err = builder.Serialize()
	s.Nil(err)

	// Fail update
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.ConditionFailedError{}).Once()

	// Receive an updated execution and complete it
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(
		&persistence.GetWorkflowExecutionResponse{
			ExecutionInfo: &persistence.WorkflowExecutionInfo{
				WorkflowID:           "wId",
				RunID:                "rId",
				TaskList:             "tId",
				History:              history,
				ExecutionContext:     nil,
				State:                3,
				NextEventID:          1,
				LastProcessedEvent:   0,
				LastUpdatedTimestamp: time.Time{},
				DecisionPending:      false},
		},
		nil).Once()

	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	task, err := s.mockMatchingEngine.pollForActivityTaskOperation(&workflow.PollForActivityTaskRequest{TaskList: taskList, Identity: &identity})
	s.Nil(err)
	s.Equal("draw head", task.GetActivityType().GetName())
	s.Equal([]byte{1, 2, 3}, task.Input)
	var updateRequest *persistence.UpdateWorkflowExecutionRequest
	ok := false
	if updateRequest, ok = s.mockExecutionMgr.Calls[len(s.mockExecutionMgr.Calls)-1].Arguments[0].(*persistence.UpdateWorkflowExecutionRequest); !ok {
		s.Fail("Wrong parameter type was passed to UpdateWorkflowExecution")
	}

	// Examine history
	updatedHistory, err := newJSONHistorySerializer().Deserialize(updateRequest.ExecutionInfo.History)
	s.Nil(err)

	// We should have 4 events: Decision, ScheduledTask and 2 StartedTasks
	s.Equal(4, len(updatedHistory))
	s.Equal(0, len(updateRequest.TransferTasks))
}

func (s *matchingEngineSuite) TestPollForDecisionTasksNoTasks() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(&persistence.GetTasksResponse{}, nil)

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.Equal(errNoTasks, err)
}

func (s *matchingEngineSuite) TestPollForDecisionTasksIfGetTaskFailed() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(nil, errors.New("Failed!!!"))

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.EqualError(err, "Failed!!!")
}

func (s *matchingEngineSuite) TestPollForDecisionTasksIfNoExecution() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"tId", &persistence.TaskInfo{
			WorkflowID:     "wId",
			RunID:          "rId",
			TaskID:         int64(1),
			TaskList:       tl,
			TaskType:       persistence.TaskTypeDecision,
			ScheduleID:     2,
			VisibilityTime: time.Time{},
			LockToken:      "lock",
			DeliveryCount:  0,
		}}},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(nil, &workflow.EntityNotExistsError{}).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.IsType(&workflow.EntityNotExistsError{}, err)
}

func (s *matchingEngineSuite) TestPollForDecisionTasksIfTaskAlreadyStarted() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	addWorkflowExecutionStartedEvent(builder, "wId", "wType", tl, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, tl, 100)
	addDecisionTaskStartedEvent(builder, scheduleEvent.GetEventId(), tl, identity)
	history, _ := builder.Serialize()

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"tId", &persistence.TaskInfo{
			WorkflowID:     "wId",
			RunID:          "rId",
			TaskID:         int64(1),
			TaskList:       tl,
			TaskType:       persistence.TaskTypeDecision,
			ScheduleID:     scheduleEvent.GetEventId(),
			VisibilityTime: time.Time{},
			LockToken:      "lock",
			DeliveryCount:  0,
		}}},
	}
	wfResponse := &persistence.GetWorkflowExecutionResponse{
		ExecutionInfo: &persistence.WorkflowExecutionInfo{
			WorkflowID:           "wId",
			RunID:                "rId",
			TaskList:             tl,
			History:              history,
			ExecutionContext:     nil,
			State:                persistence.WorkflowStateRunning,
			NextEventID:          builder.nextEventID,
			LastProcessedEvent:   emptyEventID,
			LastUpdatedTimestamp: time.Time{},
			DecisionPending:      true},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse, nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.Equal(errDuplicate, err)
}

func (s *matchingEngineSuite) TestPollForDecisionTasksIfTaskAlreadyCompleted() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	addWorkflowExecutionStartedEvent(builder, "wId", "wType", tl, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, tl, 100)
	startedEvent := addDecisionTaskStartedEvent(builder, scheduleEvent.GetEventId(), tl, identity)
	addDecisionTaskCompletedEvent(builder, scheduleEvent.GetEventId(), startedEvent.GetEventId(), nil, identity)

	history, _ := builder.Serialize()

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"tId",
			&persistence.TaskInfo{
				WorkflowID:     "wId",
				RunID:          "rId",
				TaskID:         int64(1),
				TaskList:       tl,
				TaskType:       persistence.TaskTypeDecision,
				ScheduleID:     scheduleEvent.GetEventId(),
				VisibilityTime: time.Time{},
				LockToken:      "lock",
				DeliveryCount:  0,
			}}},
	}
	wfResponse := &persistence.GetWorkflowExecutionResponse{
		ExecutionInfo: &persistence.WorkflowExecutionInfo{
			WorkflowID:           "wId",
			RunID:                "rId",
			TaskList:             tl,
			History:              history,
			ExecutionContext:     nil,
			State:                persistence.WorkflowStateRunning,
			NextEventID:          builder.nextEventID,
			LastProcessedEvent:   emptyEventID,
			LastUpdatedTimestamp: time.Time{},
			DecisionPending:      true},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse, nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.Equal(errDuplicate, err)
}

func (s *matchingEngineSuite) TestPollForDecisionTasksConflict() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	addWorkflowExecutionStartedEvent(builder, "wId", "wType", tl, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, tl, 100)
	history, _ := builder.Serialize()

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"tId",
			&persistence.TaskInfo{
				WorkflowID:     "wId",
				RunID:          "rId",
				TaskID:         int64(1),
				TaskList:       tl,
				TaskType:       persistence.TaskTypeDecision,
				ScheduleID:     scheduleEvent.GetEventId(),
				VisibilityTime: time.Time{},
				LockToken:      "lock",
				DeliveryCount:  0,
			}}},
	}
	wfResponse1 := &persistence.GetWorkflowExecutionResponse{
		ExecutionInfo: &persistence.WorkflowExecutionInfo{
			WorkflowID:           "wId",
			RunID:                "rId",
			TaskList:             tl,
			History:              history,
			ExecutionContext:     nil,
			State:                persistence.WorkflowStateRunning,
			NextEventID:          builder.nextEventID,
			LastProcessedEvent:   emptyEventID,
			LastUpdatedTimestamp: time.Time{},
			DecisionPending:      true},
	}
	wfResponse2 := &persistence.GetWorkflowExecutionResponse{
		ExecutionInfo: &persistence.WorkflowExecutionInfo{
			WorkflowID:           "wId",
			RunID:                "rId",
			TaskList:             tl,
			History:              history,
			ExecutionContext:     nil,
			State:                persistence.WorkflowStateRunning,
			NextEventID:          builder.nextEventID,
			LastProcessedEvent:   emptyEventID,
			LastUpdatedTimestamp: time.Time{},
			DecisionPending:      true},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse1, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.ConditionFailedError{}).Once()
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse2, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	task, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.Nil(err)
	s.NotNil(task)
	s.NotEmpty(task.GetTaskToken())
	s.Equal("wId", task.GetWorkflowExecution().GetWorkflowId())
	s.Equal("rId", task.GetWorkflowExecution().GetRunId())
	s.Equal("wType", task.GetWorkflowType().GetName())
	s.False(task.IsSetPreviousStartedEventId())
	s.Equal(scheduleEvent.GetEventId()+1, task.GetStartedEventId())
	s.NotEmpty(task.GetHistory())
}

func (s *matchingEngineSuite) TestPollForDecisionTasksMaxAttemptsExceeded() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	addWorkflowExecutionStartedEvent(builder, "wId", "wType", tl, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, tl, 100)
	history, _ := builder.Serialize()

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"rId",
			&persistence.TaskInfo{
				WorkflowID:     "wId",
				RunID:          "rId",
				TaskID:         int64(1),
				TaskList:       tl,
				TaskType:       persistence.TaskTypeDecision,
				ScheduleID:     scheduleEvent.GetEventId(),
				VisibilityTime: time.Time{},
				LockToken:      "lock",
				DeliveryCount:  0,
			}}},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	for i := 0; i < conditionalRetryCount; i++ {
		s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{
			ExecutionInfo: &persistence.WorkflowExecutionInfo{
				WorkflowID:           "wId",
				RunID:                "rId",
				TaskList:             tl,
				History:              history,
				ExecutionContext:     nil,
				State:                persistence.WorkflowStateRunning,
				NextEventID:          builder.nextEventID,
				LastProcessedEvent:   emptyEventID,
				LastUpdatedTimestamp: time.Time{},
				DecisionPending:      true},
		}, nil).Once()
	}
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(&persistence.ConditionFailedError{}).
		Times(conditionalRetryCount)
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	_, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.NotNil(err)
	s.Equal(errMaxAttemptsExceeded, err)
}

func (s *matchingEngineSuite) TestPollForDecisionTasksSuccess() {
	tl := "testTaskList"
	identity := "testIdentity"

	taskList := workflow.NewTaskList()
	taskList.Name = &tl

	taskRequest := &persistence.GetTasksRequest{
		TaskList:    tl,
		TaskType:    persistence.TaskTypeDecision,
		LockTimeout: taskLockDuration,
		BatchSize:   1,
	}

	builder := newHistoryBuilder(nil, bark.NewLoggerFromLogrus(log.New()))
	addWorkflowExecutionStartedEvent(builder, "wId", "wType", tl, []byte("input"), 100, 200, identity)
	scheduleEvent := addDecisionTaskScheduledEvent(builder, tl, 100)
	history, _ := builder.Serialize()

	taskResponse := &persistence.GetTasksResponse{
		Tasks: []*persistence.TaskInfoWithID{{"tId",
			&persistence.TaskInfo{
				WorkflowID:     "wId",
				RunID:          "rId",
				TaskID:         int64(1),
				TaskList:       tl,
				TaskType:       persistence.TaskTypeDecision,
				ScheduleID:     scheduleEvent.GetEventId(),
				VisibilityTime: time.Time{},
				LockToken:      "lock",
				DeliveryCount:  0,
			}}},
	}
	wfResponse := &persistence.GetWorkflowExecutionResponse{
		ExecutionInfo: &persistence.WorkflowExecutionInfo{
			WorkflowID:           "wId",
			RunID:                "rId",
			TaskList:             tl,
			History:              history,
			ExecutionContext:     nil,
			State:                persistence.WorkflowStateRunning,
			NextEventID:          builder.nextEventID,
			LastProcessedEvent:   emptyEventID,
			LastUpdatedTimestamp: time.Time{},
			DecisionPending:      true},
	}

	s.mockTaskMgr.On("GetTasks", taskRequest).Return(taskResponse, nil)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything).Return(wfResponse, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything).Return(nil).Once()
	s.mockTaskMgr.On("CompleteTask", mock.Anything).Return(nil).Once()

	task, err := s.mockMatchingEngine.pollForDecisionTaskOperation(&workflow.PollForDecisionTaskRequest{
		TaskList: taskList,
		Identity: &identity,
	})
	s.Nil(err)
	s.NotNil(task)
	s.NotEmpty(task.GetTaskToken())
	s.Equal("wId", task.GetWorkflowExecution().GetWorkflowId())
	s.Equal("rId", task.GetWorkflowExecution().GetRunId())
	s.Equal("wType", task.GetWorkflowType().GetName())
	s.False(task.IsSetPreviousStartedEventId())
	s.Equal(scheduleEvent.GetEventId()+1, task.GetStartedEventId())
	s.NotEmpty(task.GetHistory())
}