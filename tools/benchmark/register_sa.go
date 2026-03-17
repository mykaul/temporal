package main

import (
	"context"
	"fmt"
	"log"

	enumspb "go.temporal.io/api/enums/v1"
	operatorpb "go.temporal.io/api/operatorservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	conn, err := grpc.NewClient("127.0.0.1:7233", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	client := operatorpb.NewOperatorServiceClient(conn)
	_, err = client.AddSearchAttributes(context.Background(), &operatorpb.AddSearchAttributesRequest{
		SearchAttributes: map[string]enumspb.IndexedValueType{
			"OmesExecutionID": enumspb.INDEXED_VALUE_TYPE_KEYWORD,
			"KS_Keyword":      enumspb.INDEXED_VALUE_TYPE_KEYWORD,
			"KS_Int":          enumspb.INDEXED_VALUE_TYPE_INT,
		},
		Namespace: "default",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Search attributes registered successfully")
}
