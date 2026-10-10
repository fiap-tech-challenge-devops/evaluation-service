package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Evento que será enviado para a fila
type EvaluationEvent struct {
	UserID    string    `json:"user_id"`
	FlagName  string    `json:"flag_name"`
	Result    bool      `json:"result"`
	Timestamp time.Time `json:"timestamp"`
}

// sendEvaluationEvent envia um evento para a fila SQS
func (a *App) sendEvaluationEvent(ctx context.Context, userID, flagName string, result bool) {
	// Se a URL da fila não foi configurada, apenas loga localmente e sai.
	if a.SqsSvc == nil || a.SqsQueueURL == "" {
		slog.InfoContext(ctx, "[SQS_DISABLED] Evento nao enviado",
			"user_id", userID, "flag", flagName, "resultado", result)
		return
	}

	queueName := a.SqsQueueURL[strings.LastIndex(a.SqsQueueURL, "/")+1:]

	ctx, span := otel.Tracer(serviceName).Start(ctx, "send "+queueName,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(
			attribute.String("messaging.system", "aws_sqs"),
			attribute.String("messaging.operation.name", "send"),
			attribute.String("messaging.destination.name", queueName),
		),
	)
	defer span.End()

	event := EvaluationEvent{
		UserID:    userID,
		FlagName:  flagName,
		Result:    result,
		Timestamp: time.Now().UTC(),
	}

	body, err := json.Marshal(event)
	if err != nil {
		slog.ErrorContext(ctx, "Erro ao serializar evento SQS", "erro", err)
		span.RecordError(err)
		return
	}

	// O SDK v1 da AWS nao tem instrumentacao oficial (otelaws so cobre o v2),
	// entao o traceparent vai a mao nos atributos da mensagem.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	messageAttributes := make(map[string]*sqs.MessageAttributeValue, len(carrier))
	for key, value := range carrier {
		messageAttributes[key] = &sqs.MessageAttributeValue{
			DataType:    aws.String("String"),
			StringValue: aws.String(value),
		}
	}

	// Envia a mensagem
	_, err = a.SqsSvc.SendMessageWithContext(ctx, &sqs.SendMessageInput{
		MessageBody:       aws.String(string(body)),
		QueueUrl:          aws.String(a.SqsQueueURL),
		MessageAttributes: messageAttributes,
	})

	if err != nil {
		slog.ErrorContext(ctx, "Erro ao enviar mensagem para SQS", "erro", err)
		span.RecordError(err)
	} else {
		slog.InfoContext(ctx, "Evento de avaliacao enviado para SQS", "flag", flagName)
	}
}
