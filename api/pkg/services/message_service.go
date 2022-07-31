package services

import (
	"context"
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"

	"github.com/nyaruka/phonenumbers"

	"github.com/NdoleStudio/httpsms/pkg/events"
	"github.com/NdoleStudio/httpsms/pkg/repositories"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
	"github.com/palantir/stacktrace"

	"github.com/NdoleStudio/httpsms/pkg/entities"
	"github.com/NdoleStudio/httpsms/pkg/telemetry"
)

// MessageService is handles message requests
type MessageService struct {
	logger          telemetry.Logger
	tracer          telemetry.Tracer
	eventDispatcher *EventDispatcher
	repository      repositories.MessageRepository
}

// NewMessageService creates a new MessageService
func NewMessageService(
	logger telemetry.Logger,
	tracer telemetry.Tracer,
	repository repositories.MessageRepository,
	eventDispatcher *EventDispatcher,
) (s *MessageService) {
	return &MessageService{
		logger:          logger.WithService(fmt.Sprintf("%T", s)),
		tracer:          tracer,
		repository:      repository,
		eventDispatcher: eventDispatcher,
	}
}

// MessageGetOutstandingParams parameters for sending a new message
type MessageGetOutstandingParams struct {
	Source    string
	UserID    entities.UserID
	Timestamp time.Time
	MessageID uuid.UUID
}

// GetOutstanding fetches messages that still to be sent to the phone
func (service *MessageService) GetOutstanding(ctx context.Context, params MessageGetOutstandingParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.GetOutstanding(ctx, params.UserID, params.MessageID)
	if err != nil {
		msg := fmt.Sprintf("could not fetch outstanding messages with params [%s]", spew.Sdump(params))
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	event, err := service.createMessagePhoneSendingEvent(params.Source, events.MessagePhoneSendingPayload{
		ID:        message.ID,
		Owner:     message.Owner,
		Contact:   message.Contact,
		Timestamp: params.Timestamp,
		UserID:    message.UserID,
		Content:   message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create [%T] for message with ID [%s]", event, message.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID))

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("dispatched event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID))
	return message, nil
}

// MessageGetParams parameters for sending a new message
type MessageGetParams struct {
	repositories.IndexParams
	UserID  entities.UserID
	Owner   string
	Contact string
}

// GetMessages fetches sent between 2 phone numbers
func (service *MessageService) GetMessages(ctx context.Context, params MessageGetParams) (*[]entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	messages, err := service.repository.Index(ctx, params.UserID, params.Owner, params.Contact, params.IndexParams)
	if err != nil {
		msg := fmt.Sprintf("could not fetch messages with parms [%+#v]", params)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched [%d] messages with prams [%+#v]", len(*messages), params))
	return messages, nil
}

// GetMessage fetches a message by the ID
func (service *MessageService) GetMessage(ctx context.Context, userID entities.UserID, messageID uuid.UUID) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	message, err := service.repository.Load(ctx, userID, messageID)
	if err != nil {
		msg := fmt.Sprintf("could not fetch messages with ID [%s]", messageID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.PropagateWithCode(err, stacktrace.GetCode(err), msg))
	}

	return message, nil
}

// MessageStoreEventParams parameters registering a message event
type MessageStoreEventParams struct {
	MessageID    uuid.UUID
	EventName    entities.MessageEventName
	Timestamp    time.Time
	ErrorMessage *string
	Source       string
}

// StoreEvent handles event generated by a mobile phone
func (service *MessageService) StoreEvent(ctx context.Context, message *entities.Message, params MessageStoreEventParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	var err error

	switch params.EventName {
	case entities.MessageEventNameSent:
		err = service.handleMessageSentEvent(ctx, params, message)
	case entities.MessageEventNameDelivered:
		err = service.handleMessageDeliveredEvent(ctx, params, message)
	case entities.MessageEventNameFailed:
		err = service.handleMessageFailedEvent(ctx, params, message)
	default:
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.NewError(fmt.Sprintf("cannot handle message event [%s]", params.EventName)))
	}

	if err != nil {
		msg := fmt.Sprintf("could not handle phone event [%s] for message with id [%s]", params.EventName, message.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	return service.repository.Load(ctx, message.UserID, params.MessageID)
}

// MessageReceiveParams parameters registering a message event
type MessageReceiveParams struct {
	Contact   string
	UserID    entities.UserID
	Owner     phonenumbers.PhoneNumber
	Content   string
	Timestamp time.Time
	Source    string
}

// ReceiveMessage handles message received by a mobile phone
func (service *MessageService) ReceiveMessage(ctx context.Context, params MessageReceiveParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	eventPayload := events.MessagePhoneReceivedPayload{
		ID:        uuid.New(),
		UserID:    params.UserID,
		Owner:     phonenumbers.Format(&params.Owner, phonenumbers.E164),
		Contact:   params.Contact,
		Timestamp: params.Timestamp,
		Content:   params.Content,
	}

	ctxLogger.Info(fmt.Sprintf("creating cloud event for received with ID [%s]", eventPayload.ID))

	event, err := service.createMessagePhoneReceivedEvent(params.Source, eventPayload)
	if err != nil {
		msg := fmt.Sprintf("cannot create %T from payload with message id [%s]", event, eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] and message id [%s]", event.Type(), event.ID(), eventPayload.ID))

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("event [%s] dispatched succesfully", event.ID()))

	message, err := service.repository.Load(ctx, params.UserID, eventPayload.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot load message with ID [%s]", eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched message with id [%s]", message.ID))

	return message, nil
}

func (service *MessageService) handleMessageSentEvent(ctx context.Context, params MessageStoreEventParams, message *entities.Message) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	event, err := service.createMessagePhoneSentEvent(params.Source, events.MessagePhoneSentPayload{
		ID:        message.ID,
		Owner:     message.Owner,
		UserID:    message.UserID,
		Timestamp: params.Timestamp,
		Contact:   message.Contact,
		Content:   message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message [%s]", events.EventTypeMessagePhoneSent, message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}
	return nil
}

func (service *MessageService) handleMessageDeliveredEvent(ctx context.Context, params MessageStoreEventParams, message *entities.Message) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	event, err := service.createMessagePhoneDeliveredEvent(params.Source, events.MessagePhoneDeliveredPayload{
		ID:        message.ID,
		Owner:     message.Owner,
		UserID:    message.UserID,
		Timestamp: params.Timestamp,
		Contact:   message.Contact,
		Content:   message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message [%s]", events.EventTypeMessagePhoneSent, message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}
	return nil
}

func (service *MessageService) handleMessageFailedEvent(ctx context.Context, params MessageStoreEventParams, message *entities.Message) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	errorMessage := "UNKNOWN ERROR"
	if params.ErrorMessage != nil {
		errorMessage = *params.ErrorMessage
	}

	event, err := service.createMessageSendFailedEvent(params.Source, events.MessageSendFailedPayload{
		ID:           message.ID,
		Owner:        message.Owner,
		ErrorMessage: errorMessage,
		Timestamp:    params.Timestamp,
		Contact:      message.Contact,
		UserID:       message.UserID,
		Content:      message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message [%s]", events.EventTypeMessageSendFailed, message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}
	return nil
}

// MessageSendParams parameters for sending a new message
type MessageSendParams struct {
	Owner             phonenumbers.PhoneNumber
	Contact           phonenumbers.PhoneNumber
	Content           string
	Source            string
	UserID            entities.UserID
	RequestReceivedAt time.Time
}

// SendMessage a new message
func (service *MessageService) SendMessage(ctx context.Context, params MessageSendParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	eventPayload := events.MessageAPISentPayload{
		ID:                uuid.New(),
		UserID:            params.UserID,
		Owner:             phonenumbers.Format(&params.Owner, phonenumbers.E164),
		Contact:           phonenumbers.Format(&params.Contact, phonenumbers.E164),
		RequestReceivedAt: params.RequestReceivedAt,
		Content:           params.Content,
	}

	ctxLogger.Info(fmt.Sprintf("creating cloud event for message with ID [%s]", eventPayload.ID))

	event, err := service.createMessageAPISentEvent(params.Source, eventPayload)
	if err != nil {
		msg := fmt.Sprintf("cannot create %T from payload with message id [%s]", event, eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] and message id [%s]", event.Type(), event.ID(), eventPayload.ID))

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("event [%s] dispatched succesfully", event.ID()))

	message, err := service.repository.Load(ctx, eventPayload.UserID, eventPayload.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot load message with ID [%s] in the userRepository", eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched message with id [%s] from the userRepository", message.ID))

	return message, nil
}

// MessageStoreParams are parameters for creating a new message
type MessageStoreParams struct {
	Owner     string
	Contact   string
	Content   string
	UserID    entities.UserID
	ID        uuid.UUID
	Timestamp time.Time
}

// StoreSentMessage a new message
func (service *MessageService) StoreSentMessage(ctx context.Context, params MessageStoreParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message := &entities.Message{
		ID:                params.ID,
		Owner:             params.Owner,
		Contact:           params.Contact,
		UserID:            params.UserID,
		Content:           params.Content,
		Type:              entities.MessageTypeMobileTerminated,
		Status:            entities.MessageStatusPending,
		RequestReceivedAt: params.Timestamp,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		OrderTimestamp:    params.Timestamp,
		SendDuration:      nil,
		LastAttemptedAt:   nil,
		SentAt:            nil,
		ReceivedAt:        nil,
	}

	if err := service.repository.Store(ctx, message); err != nil {
		msg := fmt.Sprintf("cannot save message with id [%s]", params.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message saved with id [%s] in the userRepository", message.ID))
	return message, nil
}

// StoreReceivedMessage a new message
func (service *MessageService) StoreReceivedMessage(ctx context.Context, params MessageStoreParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message := &entities.Message{
		ID:                params.ID,
		Owner:             params.Owner,
		UserID:            params.UserID,
		Contact:           params.Contact,
		Content:           params.Content,
		Type:              entities.MessageTypeMobileOriginated,
		Status:            entities.MessageStatusReceived,
		RequestReceivedAt: params.Timestamp,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		OrderTimestamp:    params.Timestamp,
		ReceivedAt:        &params.Timestamp,
	}

	if err := service.repository.Store(ctx, message); err != nil {
		msg := fmt.Sprintf("cannot save message with id [%s]", params.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message saved with id [%s] in the userRepository", message.ID))
	return message, nil
}

// HandleMessageParams are parameters for handling a message event
type HandleMessageParams struct {
	ID        uuid.UUID
	UserID    entities.UserID
	Timestamp time.Time
}

// HandleMessageSending handles when a message is being sent
func (service *MessageService) HandleMessageSending(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSending() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected %s", message.Status, entities.MessageStatusSending)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.AddSendAttempt(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] after sending", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] in the userRepository after adding send attempt", message.ID))
	return nil
}

// HandleMessageSent handles when a message has been sent by a mobile phone
func (service *MessageService) HandleMessageSent(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSending() && !message.IsExpired() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected [%s, %s]", message.Status, entities.MessageStatusSending, entities.MessageStatusExpired)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.Sent(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] as sent", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] has been updated to status [%s]", message.ID, message.Status))
	return nil
}

// HandleMessageFailedParams are parameters for handling a failed message event
type HandleMessageFailedParams struct {
	ID           uuid.UUID
	UserID       entities.UserID
	ErrorMessage string
	Timestamp    time.Time
}

// HandleMessageFailed handles when a message could not be sent by a mobile phone
func (service *MessageService) HandleMessageFailed(ctx context.Context, params HandleMessageFailedParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if message.IsDelivered() {
		msg := fmt.Sprintf("message has already been delivered with status [%s]", message.Status)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.Failed(params.Timestamp, params.ErrorMessage)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] as sent", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] has been updated to status [%s]", message.ID, message.Status))
	return nil
}

// HandleMessageDelivered handles when a message is has been delivered by a mobile phone
func (service *MessageService) HandleMessageDelivered(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSent() && !message.IsSending() && !message.IsExpired() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected [%s, %s, %s]", message.Status, entities.MessageStatusSent, entities.MessageStatusSending, entities.MessageStatusExpired)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.Delivered(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] as delivered", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] has been updated to status [%s]", message.ID, message.Status))
	return nil
}

// HandleMessageExpired handles when a message is has been expired
func (service *MessageService) HandleMessageExpired(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSending() && !message.IsPending() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected [%s, %s]", message.Status, entities.MessageStatusSending, entities.MessageStatusSending)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.Expired(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] as expired", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] has been updated to status [%s]", message.ID, message.Status))
	return nil
}

// MessageScheduleExpirationParams are parameters for scheduling the expiration of a message event
type MessageScheduleExpirationParams struct {
	MessageID                 uuid.UUID
	UserID                    entities.UserID
	NotificationSentAt        time.Time
	PhoneID                   uuid.UUID
	MessageExpirationDuration time.Duration
	Source                    string
}

// ScheduleExpirationCheck schedules an event to check if a message is expired
func (service *MessageService) ScheduleExpirationCheck(ctx context.Context, params MessageScheduleExpirationParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	if params.MessageExpirationDuration == 0 {
		ctxLogger.Info(fmt.Sprintf("message expiration duration not set for message [%s] using phone [%s]", params.MessageID, params.PhoneID))
		return nil
	}

	event, err := service.createMessageSendExpiredCheckEvent(params.Source, events.MessageSendExpiredCheckPayload{
		MessageID:   params.MessageID,
		ScheduledAt: params.NotificationSentAt.Add(params.MessageExpirationDuration),
		UserID:      params.UserID,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message with id [%s]", events.EventTypeMessageSendExpiredCheck, params.MessageID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.DispatchWithTimeout(ctx, event, params.MessageExpirationDuration); err != nil {
		msg := fmt.Sprintf("cannot dispatch event [%s] for message with ID [%s]", event.Type(), params.MessageID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("scheduled message id [%s] to expire at [%s]", params.MessageID, params.NotificationSentAt.Add(params.MessageExpirationDuration)))
	return nil
}

// MessageCheckExpired are parameters for checking if a message is expired
type MessageCheckExpired struct {
	MessageID uuid.UUID
	UserID    entities.UserID
	Source    string
}

// CheckExpired checks if a message has expired
func (service *MessageService) CheckExpired(ctx context.Context, params MessageCheckExpired) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.UserID, params.MessageID)
	if err != nil {
		msg := fmt.Sprintf("cannot load message with userID [%s] and messageID [%s]", params.UserID, params.MessageID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsPending() && !message.IsSending() {
		ctxLogger.Info(fmt.Sprintf("message with ID [%s] has status [%s] and is not expired", message.ID, message.Status))
		return nil
	}

	event, err := service.createMessageSendExpiredEvent(params.Source, events.MessageSendExpiredPayload{
		MessageID: message.ID,
		Owner:     message.Owner,
		Contact:   message.Contact,
		UserID:    message.UserID,
		Timestamp: time.Now().UTC(),
		Content:   message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message with id [%s]", events.EventTypeMessageSendExpired, params.MessageID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event [%s] for message with ID [%s]", event.Type(), params.MessageID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message [%s] has expired with status [%s]", params.MessageID, message.Status))
	return nil
}

func (service *MessageService) createMessageSendExpiredEvent(source string, payload events.MessageSendExpiredPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessageSendExpired, source, payload)
}

func (service *MessageService) createMessageSendExpiredCheckEvent(source string, payload events.MessageSendExpiredCheckPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessageSendExpiredCheck, source, payload)
}

func (service *MessageService) createMessageAPISentEvent(source string, payload events.MessageAPISentPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessageAPISent, source, payload)
}

func (service *MessageService) createMessagePhoneReceivedEvent(source string, payload events.MessagePhoneReceivedPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneReceived, source, payload)
}

func (service *MessageService) createMessagePhoneSendingEvent(source string, payload events.MessagePhoneSendingPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneSending, source, payload)
}

func (service *MessageService) createMessagePhoneSentEvent(source string, payload events.MessagePhoneSentPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneSent, source, payload)
}

func (service *MessageService) createMessageSendFailedEvent(source string, payload events.MessageSendFailedPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessageSendFailed, source, payload)
}

func (service *MessageService) createHeartbeatPhoneOutstandingEvent(source string, payload events.HeartbeatPhoneOutstandingPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeHeartbeatPhoneOutstanding, source, payload)
}

func (service *MessageService) createMessagePhoneDeliveredEvent(source string, payload events.MessagePhoneDeliveredPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneDelivered, source, payload)
}

func (service *MessageService) createEvent(eventType string, source string, payload any) (cloudevents.Event, error) {
	event := cloudevents.NewEvent()

	event.SetSource(source)
	event.SetType(eventType)
	event.SetTime(time.Now().UTC())
	event.SetID(uuid.New().String())

	if err := event.SetData(cloudevents.ApplicationJSON, payload); err != nil {
		msg := fmt.Sprintf("cannot encode %T [%#+v] as JSON", payload, payload)
		return event, stacktrace.Propagate(err, msg)
	}

	return event, nil
}
