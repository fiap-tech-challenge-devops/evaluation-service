package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const serviceName = "evaluation-service"

// App struct para injeção de dependência
type App struct {
	RedisClient         *redis.Client
	SqsSvc              *sqs.SQS
	SqsQueueURL         string
	HttpClient          *http.Client
	FlagServiceURL      string
	TargetingServiceURL string
}

func main() {
	_ = godotenv.Load() // Carrega .env para dev local

	ctx := context.Background()

	shutdownOTel, err := setupOTel(ctx)
	if err != nil {
		slog.Error("Não foi possível inicializar a telemetria", "erro", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownOTel(shutdownCtx); err != nil {
			slog.Error("Erro no shutdown da telemetria", "erro", err)
		}
	}()

	setupLogger(serviceName)

	// --- Configuração ---
	port := os.Getenv("PORT")
	if port == "" {
		port = "8004"
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		slog.Error("REDIS_URL deve ser definida (ex: redis://localhost:6379)")
		os.Exit(1)
	}

	flagSvcURL := os.Getenv("FLAG_SERVICE_URL")
	if flagSvcURL == "" {
		slog.Error("FLAG_SERVICE_URL deve ser definida")
		os.Exit(1)
	}

	targetingSvcURL := os.Getenv("TARGETING_SERVICE_URL")
	if targetingSvcURL == "" {
		slog.Error("TARGETING_SERVICE_URL deve ser definida")
		os.Exit(1)
	}

	// SQS é opcional no dev local, mas obrigatório em prod
	sqsQueueURL := os.Getenv("AWS_SQS_URL")
	awsRegion := os.Getenv("AWS_REGION")
	if sqsQueueURL == "" {
		slog.Warn("AWS_SQS_URL não definida. Eventos não serão enviados.")
	}
	if awsRegion == "" && sqsQueueURL != "" {
		slog.Error("AWS_REGION deve ser definida para usar SQS")
		os.Exit(1)
	}

	// --- Inicializa Clientes ---

	// Cliente Redis
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		slog.Error("Não foi possível parsear a URL do Redis", "erro", err)
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	if err := redisotel.InstrumentTracing(rdb); err != nil {
		slog.Error("Não foi possível instrumentar o Redis", "erro", err)
		os.Exit(1)
	}
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		slog.Error("Não foi possível conectar ao Redis", "erro", err)
		os.Exit(1)
	}
	slog.Info("Conectado ao Redis com sucesso")

	// Cliente SQS (AWS SDK)
	var sqsSvc *sqs.SQS
	if sqsQueueURL != "" {
		sess, err := session.NewSession(&aws.Config{Region: aws.String(awsRegion)})
		if err != nil {
			slog.Error("Não foi possível criar sessão AWS", "erro", err)
			os.Exit(1)
		}
		sqsSvc = sqs.New(sess)
		slog.Info("Cliente SQS inicializado com sucesso")
	}

	// Cliente HTTP: o transport instrumentado propaga o traceparent nas chamadas
	// ao flag-service e ao targeting-service.
	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}

	// Cria a instância da App
	app := &App{
		RedisClient:         rdb,
		SqsSvc:              sqsSvc,
		SqsQueueURL:         sqsQueueURL,
		HttpClient:          httpClient,
		FlagServiceURL:      flagSvcURL,
		TargetingServiceURL: targetingSvcURL,
	}

	// --- Rotas ---
	mux := http.NewServeMux()
	mux.HandleFunc("/health", app.healthHandler)
	mux.HandleFunc("/evaluate", app.evaluationHandler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           instrument(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("Serviço de Avaliação (Go) iniciado", "porta", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Servidor encerrou com erro", "erro", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("Encerrando o serviço")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("Erro no shutdown do servidor", "erro", err)
	}
}

// instrument embrulha o mux com o handler do OpenTelemetry: nomeia o span pela
// rota e descarta o /health, que o healthcheck bate a cada 30s.
func instrument(mux *http.ServeMux) http.Handler {
	return otelhttp.NewHandler(mux, "",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/health"
		}),
	)
}
