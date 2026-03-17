package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// verify_sa validates that search attributes are usable end-to-end by
// starting a workflow with OmesExecutionID set. This only verifies the
// frontend service SA cache; history service shards have their own cache
// that may not be refreshed yet. For a fully clean benchmark, restart the
// Temporal server after registering SAs (see benchmarking.md).
func main() {
	conn, err := grpc.NewClient("127.0.0.1:7233", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := workflowpb.NewWorkflowServiceClient(conn)

	data, _ := json.Marshal("verify-sa-test")
	p := &commonpb.Payload{
		Metadata: map[string][]byte{"encoding": []byte("json/plain")},
		Data:     data,
	}

	for i := 0; i < 120; i++ {
		_, err := client.StartWorkflowExecution(context.Background(), &workflowpb.StartWorkflowExecutionRequest{
			Namespace:  "default",
			WorkflowId: "sa-verify-probe",
			WorkflowType: &commonpb.WorkflowType{
				Name: "sa-verify-probe",
			},
			TaskQueue: &taskqueuepb.TaskQueue{
				Name: "sa-verify-probe",
				Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
			},
			SearchAttributes: &commonpb.SearchAttributes{
				IndexedFields: map[string]*commonpb.Payload{
					"OmesExecutionID": p,
				},
			},
		})
		if err == nil {
			_, _ = client.TerminateWorkflowExecution(context.Background(), &workflowpb.TerminateWorkflowExecutionRequest{
				Namespace: "default",
				WorkflowExecution: &commonpb.WorkflowExecution{
					WorkflowId: "sa-verify-probe",
				},
				Reason: "SA verification probe cleanup",
			})
			fmt.Printf("Search attributes verified end-to-end after %ds\n", i)
			return
		}
		if i%10 == 0 {
			fmt.Printf("Attempt %d: %v\n", i, err)
		}
		time.Sleep(time.Second)
	}
	log.Fatal("Search attributes not usable after 120s")
}
