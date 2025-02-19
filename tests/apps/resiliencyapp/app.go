/*
Copyright 2021 The Dapr Authors
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/golang/protobuf/ptypes/any"

	commonv1pb "github.com/dapr/dapr/pkg/proto/common/v1"
	runtimev1pb "github.com/dapr/dapr/pkg/proto/runtime/v1"

	"github.com/gorilla/mux"
	"google.golang.org/grpc"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/anypb"
)

const (
	appPort = 3000
)

type FailureMessage struct {
	ID              string         `json:"id"`
	MaxFailureCount *int           `json:"maxFailureCount,omitempty"`
	Timeout         *time.Duration `json:"timeout,omitempty"`
}

type CallRecord struct {
	Count    int
	TimeSeen time.Time
}

type PubsubResponse struct {
	// Status field for proper handling of errors form pubsub
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

var (
	daprClient   runtimev1pb.DaprClient
	callTracking map[string][]CallRecord
)

// Endpoint handling.
func indexHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("indexHandler() called")
	w.WriteHeader(http.StatusOK)
}

func configureSubscribeHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("/dapr/subscribe called")

	subscriptions := []struct {
		PubsubName string
		Topic      string
		Route      string
	}{
		{
			PubsubName: "dapr-resiliency-pubsub",
			Topic:      "resiliency-topic-http",
			Route:      "resiliency-topic-http",
		},
	}
	b, err := json.Marshal(subscriptions)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func resiliencyBindingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		log.Println("resiliency binding input has been accepted")

		w.WriteHeader(http.StatusOK)
		return
	}

	var message FailureMessage
	json.NewDecoder(r.Body).Decode(&message)

	log.Printf("Binding received message %+v\n", message)

	callCount := 0
	if records, ok := callTracking[message.ID]; ok {
		callCount = records[len(records)-1].Count + 1
	}

	log.Printf("Seen %s %d times.", message.ID, callCount)

	callTracking[message.ID] = append(callTracking[message.ID], CallRecord{Count: callCount, TimeSeen: time.Now()})
	if message.MaxFailureCount != nil && callCount < *message.MaxFailureCount {
		if message.Timeout != nil {
			// This request can still succeed if the resiliency policy timeout is longer than this sleep.
			time.Sleep(*message.Timeout)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func resiliencyPubsubHandler(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(PubsubResponse{
			Message: "No body",
			Status:  "DROP",
		})
	}
	defer r.Body.Close()

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(PubsubResponse{
			Message: "Couldn't read body",
			Status:  "DROP",
		})
	}

	log.Printf("Raw body: %s", string(body))

	var rawBody map[string]interface{}
	json.Unmarshal(body, &rawBody)

	rawData := rawBody["data"].(map[string]interface{})
	rawDataBytes, _ := json.Marshal(rawData)
	var message FailureMessage
	json.Unmarshal(rawDataBytes, &message)
	log.Printf("Pubsub received message %+v\n", message)

	callCount := 0
	if records, ok := callTracking[message.ID]; ok {
		callCount = records[len(records)-1].Count + 1
	}

	log.Printf("Seen %s %d times.", message.ID, callCount)

	callTracking[message.ID] = append(callTracking[message.ID], CallRecord{Count: callCount, TimeSeen: time.Now()})
	if message.MaxFailureCount != nil && callCount < *message.MaxFailureCount {
		if message.Timeout != nil {
			// This request can still succeed if the resiliency policy timeout is longer than this sleep.
			time.Sleep(*message.Timeout)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(PubsubResponse{
		Message: "consumed",
		Status:  "SUCCESS",
	})
}

func resiliencyServiceInvocationHandler(w http.ResponseWriter, r *http.Request) {
	var message FailureMessage
	json.NewDecoder(r.Body).Decode(&message)

	log.Printf("Http invocation received message %+v\n", message)

	callCount := 0
	if records, ok := callTracking[message.ID]; ok {
		callCount = records[len(records)-1].Count + 1
	}

	log.Printf("Seen %s %d times.", message.ID, callCount)

	callTracking[message.ID] = append(callTracking[message.ID], CallRecord{Count: callCount, TimeSeen: time.Now()})
	if message.MaxFailureCount != nil && callCount < *message.MaxFailureCount {
		if message.Timeout != nil {
			// This request can still succeed if the resiliency policy timeout is longer than this sleep.
			time.Sleep(*message.Timeout)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

// App startup/endpoint setup.
func initGRPCClient() {
	url := fmt.Sprintf("localhost:%d", 50001)
	log.Printf("Connecting to dapr using url %s", url)
	var grpcConn *grpc.ClientConn
	for retries := 10; retries > 0; retries-- {
		var err error
		grpcConn, err = grpc.Dial(url, grpc.WithInsecure())
		if err == nil {
			break
		}

		if retries == 0 {
			log.Printf("Could not connect to dapr: %v", err)
			log.Panic(err)
		}

		log.Printf("Could not connect to dapr: %v, retrying...", err)
		time.Sleep(5 * time.Second)
	}

	daprClient = runtimev1pb.NewDaprClient(grpcConn)
}

func newHTTPClient() *http.Client {
	dialer := &net.Dialer{ //nolint:exhaustivestruct
		Timeout: 5 * time.Second,
	}
	netTransport := &http.Transport{ //nolint:exhaustivestruct
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 5 * time.Second,
	}

	return &http.Client{ //nolint:exhaustivestruct
		Timeout:   30 * time.Second,
		Transport: netTransport,
	}
}

func appRouter() *mux.Router {
	router := mux.NewRouter().StrictSlash(true)

	router.HandleFunc("/", indexHandler).Methods("GET")

	// Calls from dapr.
	router.HandleFunc("/dapr/subscribe", configureSubscribeHandler).Methods("GET")

	// Handling events/methods.
	router.HandleFunc("/resiliencybinding", resiliencyBindingHandler).Methods("POST", "OPTIONS")
	router.HandleFunc("/resiliency-topic-http", resiliencyPubsubHandler).Methods("POST")
	router.HandleFunc("/resiliencyInvocation", resiliencyServiceInvocationHandler).Methods("POST")

	// Test functions.
	router.HandleFunc("/tests/getCallCount", TestGetCallCount).Methods("GET")
	router.HandleFunc("/tests/getCallCountGRPC", TestGetCallCountGRPC).Methods("GET")
	router.HandleFunc("/tests/invokeBinding/{binding}", TestInvokeOutputBinding).Methods("POST")
	router.HandleFunc("/tests/publishMessage/{pubsub}/{topic}", TestPublishMessage).Methods("POST")
	router.HandleFunc("/tests/invokeService/{protocol}", TestInvokeService).Methods("POST")

	router.Use(mux.CORSMethodMiddleware(router))

	return router
}

func main() {
	log.Printf("Hello Dapr - listening on http://localhost:%d", appPort)
	callTracking = map[string][]CallRecord{}
	initGRPCClient()

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", appPort), appRouter()))
}

// Test Functions.
func TestGetCallCount(w http.ResponseWriter, r *http.Request) {
	log.Println("Getting call counts")
	for key, val := range callTracking {
		log.Printf("\t%s - Called %d times.\n", key, len(val))
	}

	b, err := json.Marshal(callTracking)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(b)
}

func TestGetCallCountGRPC(w http.ResponseWriter, r *http.Request) {
	log.Printf("Getting call counts for gRPC")

	req := runtimev1pb.InvokeServiceRequest{
		Id: "resiliencyappgrpc",
		Message: &commonv1pb.InvokeRequest{
			Method: "GetCallCount",
			Data:   &any.Any{},
			HttpExtension: &commonv1pb.HTTPExtension{
				Verb: commonv1pb.HTTPExtension_POST,
			},
		},
	}

	resp, err := daprClient.InvokeService(context.Background(), &req)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(resp.Data.Value)
}

func TestInvokeOutputBinding(w http.ResponseWriter, r *http.Request) {
	binding := mux.Vars(r)["binding"]
	log.Printf("Making call to output binding %s.", binding)

	var message FailureMessage
	err := json.NewDecoder(r.Body).Decode(&message)
	if err != nil {
		log.Println("Could not parse message.")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	b, _ := json.Marshal(message)
	req := &runtimev1pb.InvokeBindingRequest{
		Name:      binding,
		Operation: "create",
		Data:      b,
	}

	_, err = daprClient.InvokeBinding(context.Background(), req)
	if err != nil {
		log.Printf("Error invoking binding: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestPublishMessage(w http.ResponseWriter, r *http.Request) {
	pubsub := mux.Vars(r)["pubsub"]
	topic := mux.Vars(r)["topic"]

	var message FailureMessage
	err := json.NewDecoder(r.Body).Decode(&message)
	if err != nil {
		log.Println("Could not parse message.")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	log.Printf("Publishing to %s/%s - %+v", pubsub, topic, message)
	b, _ := json.Marshal(message)

	req := &runtimev1pb.PublishEventRequest{
		PubsubName:      pubsub,
		Topic:           topic,
		Data:            b,
		DataContentType: "application/json",
	}

	_, err = daprClient.PublishEvent(context.Background(), req)
	if err != nil {
		log.Printf("Error publishing event: %s", err.Error())
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func TestInvokeService(w http.ResponseWriter, r *http.Request) {
	protocol := mux.Vars(r)["protocol"]
	log.Printf("Invoking resiliency service with %s", protocol)

	if protocol == "http" {
		client := newHTTPClient()
		url := "http://localhost:3500/v1.0/invoke/resiliencyapp/method/resiliencyInvocation"

		req, _ := http.NewRequest("POST", url, r.Body)
		defer r.Body.Close()

		resp, err := client.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(resp.StatusCode)
	} else if protocol == "grpc" {
		var message FailureMessage
		err := json.NewDecoder(r.Body).Decode(&message)
		if err != nil {
			log.Println("Could not parse message.")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, _ := json.Marshal(message)

		req := &runtimev1pb.InvokeServiceRequest{
			Id: "resiliencyappgrpc",
			Message: &commonv1pb.InvokeRequest{
				Method: "grpcInvoke",
				Data: &anypb.Any{
					Value: b,
				},
			},
		}

		_, err = daprClient.InvokeService(r.Context(), req)
		if err != nil {
			log.Printf("Failed to invoke service: %s", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else if protocol == "grpc_proxy" {
		var message FailureMessage
		err := json.NewDecoder(r.Body).Decode(&message)
		if err != nil {
			log.Println("Could not parse message.")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		log.Printf("Proxying message: %+v", message)
		b, _ := json.Marshal(message)

		conn, err := grpc.Dial("localhost:50001", grpc.WithInsecure(), grpc.WithBlock())
		if err != nil {
			log.Fatalf("did not connect: %v", err)
		}
		defer conn.Close()
		client := pb.NewGreeterClient(conn)

		ctx := r.Context()
		ctx = metadata.AppendToOutgoingContext(ctx, "dapr-app-id", "resiliencyappgrpc")
		_, err = client.SayHello(ctx, &pb.HelloRequest{Name: string(b)})
		if err != nil {
			log.Printf("could not greet: %v\n", err)
			w.WriteHeader(500)
			w.Write([]byte(fmt.Sprintf("failed to proxy request: %s", err)))
			return
		}
	}

}
