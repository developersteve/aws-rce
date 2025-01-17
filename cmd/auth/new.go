package awsrce

import (
	"context"
	"fmt"
	"os"

	"github.com/alexflint/go-arg"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	"github.com/nathants/aws-rce/rce"
	"github.com/nathants/libaws/lib"
)

func init() {
	lib.Commands["auth-new"] = authNew
	lib.Args["auth-new"] = authNewArgs{}
}

type authNewArgs struct {
	Name string `arg:"positional,required"`
}

func (authNewArgs) Description() string {
	return "\nnew auth\n"
}

func authNew() {
	var args authNewArgs
	arg.MustParse(&args)
	table := os.Getenv("PROJECT_NAME")
	key := rce.RandKey()
	item, err := dynamodbattribute.MarshalMap(rce.Record{
		RecordKey: rce.RecordKey{
			ID: fmt.Sprintf("auth.%s", rce.Blake2b32(key)),
		},
		RecordData: rce.RecordData{
			Value: args.Name,
		},
	})
	if err != nil {
		lib.Logger.Fatal("error: ", err)
	}
	err = lib.Retry(context.Background(), func() error {
		_, err := lib.DynamoDBClient().PutItem(&dynamodb.PutItemInput{
			Item:      item,
			TableName: aws.String(table),
		})
		return err
	})
	if err != nil {
		lib.Logger.Fatal("error: ", err)
	}
	fmt.Println(key)
}
