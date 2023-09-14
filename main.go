package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/honeycombio/honeycomb-network-agent/assemblers"
	"github.com/honeycombio/honeycomb-network-agent/config"
	"github.com/honeycombio/honeycomb-network-agent/debug"
	"github.com/honeycombio/honeycomb-network-agent/utils"
	"github.com/honeycombio/libhoney-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	semconv "go.opentelemetry.io/otel/semconv/v1.20.0"
)

const Version string = "0.0.13-alpha"
const defaultDataset = "hny-network-agent"
const defaultEndpoint = "https://api.honeycomb.io"

func main() {
	// Set logging level
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if os.Getenv("LOG_LEVEL") == "DEBUG" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	if os.Getenv("DEBUG") == "true" {
		debug := &debug.DebugService{}
		debug.Start()
	}
	// TODO: add a flag to enable human readable logs
	// log.Logger = log.Output(zerolog.NewConsoleWriter())

	log.Info().Str("agent_version", Version).Msg("Starting Honeycomb Network agent")

	kernelVersion, err := utils.HostKernelVersion()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to get host kernel version")
	}
	btfEnabled := utils.HostBtfEnabled()
	log.Info().
		Str("kernel_version", kernelVersion.String()).
		Bool("btf_enabled", btfEnabled).
		Msg("Detected host kernel")

	apikey := os.Getenv("HONEYCOMB_API_KEY")
	if apikey == "" {
		log.Fatal().Msg("Honeycomb API key not set, unable to send events\n")
	}

	dataset := getEnvOrDefault("HONEYCOMB_DATASET", defaultDataset)
	endpoint := getEnvOrDefault("HONEYCOMB_API_ENDPOINT", defaultEndpoint)
	log.Info().
		Str("hny_endpoint", endpoint).
		Str("hny_dataset", dataset).
		Msg("Honeycomb API config")

	// setup libhoney
	libhoney.Init(libhoney.Config{
		APIKey:  apikey,
		Dataset: dataset,
		APIHost: endpoint,
	})

	// appends libhoney's user-agent (TODO: doesn't work, no useragent right now)
	libhoney.UserAgentAddition = fmt.Sprintf("hny/network-agent/%s", Version)

	// configure global fields that are set on all events
	libhoney.AddField("honeycomb.agent_version", Version)
	libhoney.AddField("meta.kernel_version", kernelVersion.String())
	libhoney.AddField("meta.btf_enabled", btfEnabled)

	defer libhoney.Close()

	// creates the in-cluster config
	k8sConfig, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	// creates the clientset
	k8sClient, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		panic(err.Error())
	}

	// create k8s monitor that caches k8s objects
	ctx, done := context.WithCancel(context.Background())
	defer done()
	cachedK8sClient := utils.NewCachedK8sClient(k8sClient)
	cachedK8sClient.Start(ctx)

	agentConfig := config.NewConfig()

	// setup TCP stream reader
	httpEvents := make(chan assemblers.HttpEvent, agentConfig.ChannelBufferSize)
	assembler := assemblers.NewTcpAssembler(agentConfig, httpEvents)
	go handleHttpEvents(httpEvents, cachedK8sClient)
	go assembler.Start()
	defer assembler.Stop()

	log.Info().Msg("Agent is ready!")

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	<-signalChannel

	log.Info().Msg("Shutting down...")
}

func handleHttpEvents(events chan assemblers.HttpEvent, client *utils.CachedK8sClient) {
	for {
		event := <-events
		sendHttpEventToHoneycomb(event, client)
	}
}

func sendHttpEventToHoneycomb(event assemblers.HttpEvent, k8sClient *utils.CachedK8sClient) {
	// create libhoney event
	ev := libhoney.NewEvent()

	// calculate event duration
	// TODO: This is a hack to work around a bug that results in the response timestamp sometimes
	// being zero which causes the event duration to be negative.
	if event.RequestTimestamp.IsZero() {
		log.Debug().
			Str("request_id", event.RequestId).
			Msg("Request timestamp is zero")
		ev.AddField("http.request.timestamp_missing", true)
		event.RequestTimestamp = time.Now()
	}
	if event.ResponseTimestamp.IsZero() {
		log.Debug().
			Str("request_id", event.RequestId).
			Msg("Response timestamp is zero")
		ev.AddField("http.response.timestamp_missing", true)
		event.ResponseTimestamp = time.Now()
	}
	eventDuration := event.ResponseTimestamp.Sub(event.RequestTimestamp)

	// common attributes
	ev.Timestamp = event.RequestTimestamp
	ev.AddField("httpEvent_handled_at", time.Now())
	ev.AddField("meta.httpEvent_request_handled_latency_ms", time.Now().Sub(event.RequestTimestamp).Milliseconds())
	ev.AddField("meta.httpEvent_response_handled_latency_ms", time.Now().Sub(event.ResponseTimestamp).Milliseconds())
	ev.AddField("goroutine_count", runtime.NumGoroutine())
	ev.AddField("duration_ms", eventDuration.Milliseconds())
	ev.AddField("http.request.timestamp", event.RequestTimestamp)
	ev.AddField("http.response.timestamp", event.ResponseTimestamp)
	ev.AddField("http.request.id", event.RequestId)

	ev.AddField(string(semconv.NetSockHostAddrKey), event.SrcIp)
	ev.AddField("destination.address", event.DstIp)

	var requestURI string

	// request attributes
	if event.Request != nil {
		requestURI = event.Request.RequestURI

		bodySizeString := event.Request.Header.Get("Content-Length")
		bodySize, _ := strconv.ParseInt(bodySizeString, 10, 64)
		ev.AddField("name", fmt.Sprintf("HTTP %s", event.Request.Method))
		ev.AddField(string(semconv.HTTPMethodKey), event.Request.Method)
		ev.AddField(string(semconv.HTTPURLKey), requestURI)
		ev.AddField(string(semconv.UserAgentOriginalKey), event.Request.Header.Get("User-Agent"))
		ev.AddField("http.request.body.size", bodySize)
	} else {
		ev.AddField("name", "HTTP")
		ev.AddField("http.request.missing", "no request on this event")
	}

	// response attributes
	if event.Response != nil {
		bodySizeString := event.Response.Header.Get("Content-Length")
		bodySize, _ := strconv.ParseInt(bodySizeString, 10, 64)

		ev.AddField(string(semconv.HTTPStatusCodeKey), event.Response.StatusCode)
		ev.AddField("http.response.body.size", bodySize)

	} else {
		ev.AddField("http.response.missing", "no response on this event")
	}

	k8sEventAttrs := utils.GetK8sEventAttrs(k8sClient, event.SrcIp, event.DstIp)
	ev.Add(k8sEventAttrs)

	log.Debug().
		Str("request_id", event.RequestId).
		Time("event.timestamp", ev.Timestamp).
		Str("http.url", requestURI).
		Msg("Event sent")
	err := ev.Send()
	if err != nil {
		log.Debug().
			Err(err).
			Msg("error sending event")
	}
}

func getEnvOrDefault(key string, defaultValue string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return defaultValue
}
