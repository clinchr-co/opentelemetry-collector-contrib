// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package splunkhecreceiver

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"go.opencensus.io/trace"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/consumer/pdata"
	"go.opentelemetry.io/collector/obsreport"
	"go.opentelemetry.io/collector/translator/conventions"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/common/splunk"
)

const (
	defaultServerTimeout = 20 * time.Second

	responseOK                        = "OK"
	responseInvalidMethod             = `Only "POST" method is supported`
	responseInvalidContentType        = "\"Content-Type\" must be \"application/json\""
	responseInvalidEncoding           = "\"Content-Encoding\" must be \"gzip\" or empty"
	responseErrGzipReader             = "Error on gzip body"
	responseErrUnmarshalBody          = "Failed to unmarshal message body"
	responseErrNextConsumer           = "Internal Server Error"
	responseErrUnsupportedMetricEvent = "Unsupported metric event"

	// Centralizing some HTTP and related string constants.
	jsonContentType           = "application/json"
	gzipEncoding              = "gzip"
	httpContentTypeHeader     = "Content-Type"
	httpContentEncodingHeader = "Content-Encoding"
)

var (
	errNilNextConsumer = errors.New("nil metricsConsumer")
	errEmptyEndpoint   = errors.New("empty endpoint")

	okRespBody                = initJSONResponse(responseOK)
	invalidMethodRespBody     = initJSONResponse(responseInvalidMethod)
	invalidContentRespBody    = initJSONResponse(responseInvalidContentType)
	invalidEncodingRespBody   = initJSONResponse(responseInvalidEncoding)
	errGzipReaderRespBody     = initJSONResponse(responseErrGzipReader)
	errUnmarshalBodyRespBody  = initJSONResponse(responseErrUnmarshalBody)
	errNextConsumerRespBody   = initJSONResponse(responseErrNextConsumer)
	errUnsupportedMetricEvent = initJSONResponse(responseErrUnsupportedMetricEvent)
)

// splunkReceiver implements the component.MetricsReceiver for Splunk HEC metric protocol.
type splunkReceiver struct {
	sync.Mutex
	logger      *zap.Logger
	config      *Config
	logConsumer consumer.LogsConsumer
	server      *http.Server
}

var _ component.MetricsReceiver = (*splunkReceiver)(nil)

// New creates the Splunk HEC receiver with the given configuration.
func New(
	logger *zap.Logger,
	config Config,
	nextConsumer consumer.LogsConsumer,
) (component.MetricsReceiver, error) {
	if nextConsumer == nil {
		return nil, errNilNextConsumer
	}

	if config.Endpoint == "" {
		return nil, errEmptyEndpoint
	}

	r := &splunkReceiver{
		logger:      logger,
		config:      &config,
		logConsumer: nextConsumer,
		server: &http.Server{
			Addr: config.Endpoint,
			// TODO: Evaluate what properties should be configurable, for now
			//		set some hard-coded values.
			ReadHeaderTimeout: defaultServerTimeout,
			WriteTimeout:      defaultServerTimeout,
		},
	}

	return r, nil
}

// StartMetricsReception tells the receiver to start its processing.
// By convention the consumer of the received data is set when the receiver
// instance is created.
func (r *splunkReceiver) Start(_ context.Context, host component.Host) error {
	r.Lock()
	defer r.Unlock()

	var ln net.Listener
	// set up the listener
	ln, err := r.config.HTTPServerSettings.ToListener()
	if err != nil {
		return fmt.Errorf("failed to bind to address %s: %w", r.config.Endpoint, err)
	}

	mx := mux.NewRouter()
	mx.HandleFunc(hecPath, r.handleReq)

	r.server = r.config.HTTPServerSettings.ToServer(mx)

	// TODO: Evaluate what properties should be configurable, for now
	//		set some hard-coded values.
	r.server.ReadHeaderTimeout = defaultServerTimeout
	r.server.WriteTimeout = defaultServerTimeout

	go func() {
		if errHTTP := r.server.Serve(ln); errHTTP != nil {
			host.ReportFatalError(errHTTP)
		}
	}()

	return err
}

// StopMetricsReception tells the receiver that should stop reception,
// giving it a chance to perform any necessary clean-up.
func (r *splunkReceiver) Shutdown(context.Context) error {
	r.Lock()
	defer r.Unlock()

	err := r.server.Close()

	return err
}

func (r *splunkReceiver) handleReq(resp http.ResponseWriter, req *http.Request) {
	transport := "http"
	if r.config.TLSSetting != nil {
		transport = "https"
	}
	ctx := obsreport.ReceiverContext(req.Context(), r.config.Name(), transport, r.config.Name())
	ctx = obsreport.StartMetricsReceiveOp(ctx, r.config.Name(), transport)

	if req.Method != http.MethodPost {
		r.failRequest(ctx, resp, http.StatusBadRequest, invalidMethodRespBody, nil)
		return
	}

	if req.Header.Get(httpContentTypeHeader) != jsonContentType {
		r.failRequest(ctx, resp, http.StatusUnsupportedMediaType, invalidContentRespBody, nil)
		return
	}

	encoding := req.Header.Get(httpContentEncodingHeader)
	if encoding != "" && encoding != gzipEncoding {
		r.failRequest(ctx, resp, http.StatusUnsupportedMediaType, invalidEncodingRespBody, nil)
		return
	}

	bodyReader := req.Body
	if encoding == gzipEncoding {
		var err error
		bodyReader, err = gzip.NewReader(bodyReader)
		if err != nil {
			r.failRequest(ctx, resp, http.StatusBadRequest, errGzipReaderRespBody, err)
			return
		}
	}

	if req.ContentLength == 0 {
		resp.Write(okRespBody)
		return
	}

	messagesReceived := 0
	dec := json.NewDecoder(bodyReader)

	var events []*splunk.Event

	var decodeErr error
	for dec.More() {
		var msg splunk.Event
		err := dec.Decode(&msg)
		if err != nil {
			r.failRequest(ctx, resp, http.StatusBadRequest, errUnmarshalBodyRespBody, err)
			return
		}
		if msg.IsMetric() {
			// Currently unsupported.
			r.failRequest(ctx, resp, http.StatusBadRequest, errUnsupportedMetricEvent, err)
			return
		}

		messagesReceived++
		events = append(events, &msg)
	}

	logSlice, err := SplunkHecToLogData(r.logger, events)
	if err != nil {
		r.failRequest(ctx, resp, http.StatusBadRequest, errUnmarshalBodyRespBody, err)
		return
	}

	ld := pdata.NewLogs()
	rls := ld.ResourceLogs()
	rls.Resize(1)
	rl := rls.At(0)
	resource := rl.Resource()

	ills := rl.InstrumentationLibraryLogs()
	ills.Resize(1)
	ill := ills.At(0)

	logSlice.MoveAndAppendTo(ill.Logs())

	if r.config.AccessTokenPassthrough {
		if accessToken := req.Header.Get(splunk.SplunkHECTokenHeader); accessToken != "" {
			resource.InitEmpty()
			resource.Attributes().InsertString(splunk.SplunkHecTokenLabel, accessToken)
		}
	}
	decodeErr = r.logConsumer.ConsumeLogs(ctx, ld)

	obsreport.EndMetricsReceiveOp(
		ctx,
		typeStr,
		messagesReceived,
		messagesReceived,
		decodeErr)
	if decodeErr != nil {
		r.failRequest(ctx, resp, http.StatusInternalServerError, errNextConsumerRespBody, decodeErr)
	} else {
		resp.WriteHeader(http.StatusAccepted)
		resp.Write(okRespBody)
	}
}

func (r *splunkReceiver) failRequest(
	ctx context.Context,
	resp http.ResponseWriter,
	httpStatusCode int,
	jsonResponse []byte,
	err error,
) {
	resp.WriteHeader(httpStatusCode)
	if len(jsonResponse) > 0 {
		// The response needs to be written as a JSON string.
		_, writeErr := resp.Write(jsonResponse)
		if writeErr != nil {
			r.logger.Warn(
				"Error writing HTTP response message",
				zap.Error(writeErr),
				zap.String("receiver", r.config.Name()))
		}
	}

	msg := string(jsonResponse)

	reqSpan := trace.FromContext(ctx)
	reqSpan.AddAttributes(
		trace.Int64Attribute(conventions.AttributeHTTPStatusCode, int64(httpStatusCode)),
		trace.StringAttribute(conventions.AttributeHTTPStatusText, msg))
	traceStatus := trace.Status{
		Code: trace.StatusCodeInvalidArgument,
	}
	if httpStatusCode == http.StatusInternalServerError {
		traceStatus.Code = trace.StatusCodeInternal
	}
	if err != nil {
		traceStatus.Message = err.Error()
	}
	reqSpan.SetStatus(traceStatus)
	reqSpan.End()

	r.logger.Debug(
		"Splunk HEC receiver request failed",
		zap.Int("http_status_code", httpStatusCode),
		zap.String("msg", msg),
		zap.Error(err), // It handles nil error
		zap.String("receiver", r.config.Name()))
}

func initJSONResponse(s string) []byte {
	respBody, err := json.Marshal(s)
	if err != nil {
		// This is to be used in initialization so panic here is fine.
		panic(err)
	}
	return respBody
}
