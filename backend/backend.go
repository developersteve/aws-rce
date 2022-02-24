//
// attr: name ${PROJECT_NAME}
// attr: concurrency 0
// attr: memory 128
// attr: timeout 900
//
// dynamodb: ${PROJECT_NAME} id:s:hash
// s3: ${PROJECT_BUCKET} cors=true acl=private ttldays=1
//
// trigger: api dns=${PROJECT_DOMAIN}
// trigger: cloudwatch rate(5 minutes)
//
// policy: AWSLambdaBasicExecutionRole
// allow: dynamodb:* arn:aws:dynamodb:*:*:table/${PROJECT_NAME}
// allow: s3:* arn:aws:s3:::${PROJECT_BUCKET}/*
// allow: lambda:InvokeFunction arn:aws:lambda:*:*:function:${PROJECT_NAME}
//
// include: ../frontend/public/js/
// include: ../frontend/public/index.*
// include: ../frontend/public/favicon.*
//

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strconv"
	"sync"

	"fmt"
	"io/ioutil"
	"mime"

	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/aws/aws-sdk-go/service/dynamodb/dynamodbattribute"
	sdkLambda "github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mitchellh/mapstructure"
	"github.com/nathants/aws-rce/rce"
	"github.com/nathants/cli-aws/lib"
	uuid "github.com/satori/go.uuid"
)

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "POST, GET, OPTIONS, PUT, DELETE",
	"Access-Control-Allow-Headers": "auth, content-type",
}

func index() events.APIGatewayProxyResponse {
	headers := map[string]string{
		"Content-Type": "text/html; charset=UTF-8",
	}
	indexBytes, err := ioutil.ReadFile("frontend/public/index.html.gzip")
	if err == nil {
		headers["Content-Encoding"] = "gzip"
	} else {
		indexBytes, err = ioutil.ReadFile("frontend/public/index.html")
		if err != nil {
			panic(err)
		}
	}
	return events.APIGatewayProxyResponse{
		Body:            base64.StdEncoding.EncodeToString(indexBytes),
		IsBase64Encoded: true,
		StatusCode:      200,
		Headers:         headers,
	}
}

func static(path string) events.APIGatewayProxyResponse {
	data, err := ioutil.ReadFile("frontend/public" + path)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 404,
		}
	}
	headers := map[string]string{
		"Content-Type": mime.TypeByExtension("." + last(strings.Split(path, "."))),
	}
	var body string
	if len(data) > 4*1024*1024 {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		_, err = w.Write(data)
		if err != nil {
			panic(err)
		}
		err = w.Close()
		if err != nil {
			panic(err)
		}
		body = base64.StdEncoding.EncodeToString(buf.Bytes())
		headers["Content-Encoding"] = "gzip"
	} else {
		body = base64.StdEncoding.EncodeToString(data)
	}
	return events.APIGatewayProxyResponse{
		Body:            body,
		IsBase64Encoded: true,
		StatusCode:      200,
		Headers:         headers,
	}
}

func last(xs []string) string {
	return xs[len(xs)-1]
}

func notfound() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		Body:       "404",
		StatusCode: 404,
	}
}

func ok() events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		Body:       "ok",
		StatusCode: 200,
	}
}

func checkAuth(ctx context.Context, auth string) bool {
	key, err := dynamodbattribute.MarshalMap(rce.RecordKey{
		ID: fmt.Sprintf("auth.%s", rce.Blake2b32(auth)),
	})
	if err != nil {
		return false
	}
	table := os.Getenv("PROJECT_NAME")
	out, err := lib.DynamoDBClient().GetItemWithContext(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(table),
		ConsistentRead: aws.Bool(true),
		Key:            key,
	})
	if err != nil {
		return false
	}
	val := rce.Record{}
	err = dynamodbattribute.UnmarshalMap(out.Item, &val)
	if err != nil {
		return false
	}
	if val.Value == "" {
		return false
	}
	lib.Logger.Println("auth:", val.Value)
	return true
}

func httpExecGet(ctx context.Context, event *events.APIGatewayProxyRequest, res chan<- events.APIGatewayProxyResponse) {
	bucket := os.Getenv("PROJECT_BUCKET")
	getRequest := rce.ExecGetRequest{
		Uid:       event.QueryStringParameters["uid"],
		Increment: aws.Int(atoi(event.QueryStringParameters["increment"])),
	}
	// check for log N
	logKey := fmt.Sprintf("jobs/%s/logs.%05d", getRequest.Uid, *getRequest.Increment)
	_, err := lib.S3Client().HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(logKey),
	})
	if err == nil {
		req, _ := lib.S3Client().GetObjectRequest(&s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(logKey),
		})
		url, err := req.Presign(60 * time.Second)
		if err != nil {
			panic(err)
		}
		respData, err := json.Marshal(rce.ExecGetResponse{
			Increment: aws.Int(*getRequest.Increment + 1),
			LogUrl:    url,
		})
		if err != nil {
			panic(err)
		}
		res <- events.APIGatewayProxyResponse{
			StatusCode: 200,
			Body:       string(respData),
			Headers:    corsHeaders,
		}
		return
	}
	// on log N miss, check for exit code
	exitKey := fmt.Sprintf("jobs/%s/exit", getRequest.Uid)
	_, err = lib.S3Client().HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(exitKey),
	})
	if err == nil {

		// on exit hit, check once more for logs since log N and exit could both be written after inital miss for log N
		_, err := lib.S3Client().HeadObjectWithContext(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(logKey),
		})
		if err == nil {
			req, _ := lib.S3Client().GetObjectRequest(&s3.GetObjectInput{
				Bucket: aws.String(bucket),
				Key:    aws.String(logKey),
			})
			url, err := req.Presign(60 * time.Second)
			if err != nil {
				panic(err)
			}
			respData, err := json.Marshal(rce.ExecGetResponse{
				Increment: aws.Int(*getRequest.Increment + 1),
				LogUrl:    url,
			})
			if err != nil {
				panic(err)
			}
			res <- events.APIGatewayProxyResponse{
				StatusCode: 200,
				Body:       string(respData),
				Headers:    corsHeaders,
			}
			return
		}

		// on second log miss, return exit
		out, err := lib.S3Client().GetObjectWithContext(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(exitKey),
		})
		if err != nil {
			panic(err)
		}
		defer func() { _ = out.Body.Close() }()
		outData, err := ioutil.ReadAll(out.Body)
		if err != nil {
			panic(err)
		}
		respData, err := json.Marshal(rce.ExecGetResponse{
			ExitCode: aws.Int(atoi(string(outData))),
		})
		if err != nil {
			panic(err)
		}
		res <- events.APIGatewayProxyResponse{
			StatusCode: 200,
			Body:       string(respData),
			Headers:    corsHeaders,
		}
		return
	}

	// no data, wait
	respData, err := json.Marshal(rce.ExecGetResponse{})
	if err != nil {
		panic(err)
	}
	res <- events.APIGatewayProxyResponse{
		StatusCode: 409,
		Body:       string(respData),
		Headers:    corsHeaders,
	}
}

func httpExecPost(ctx context.Context, event *events.APIGatewayProxyRequest, res chan<- events.APIGatewayProxyResponse) {
	postReqest := rce.ExecPostRequest{}
	if event.IsBase64Encoded {
		data, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			panic(err)
		}
		event.Body = string(data)
	}
	err := json.Unmarshal([]byte(event.Body), &postReqest)
	if err != nil {
		panic(fmt.Sprint(event.Body, err))
	}
	uid := fmt.Sprintf("%d.%s", time.Now().Unix(), uuid.NewV4().String())
	data, err := json.Marshal(rce.ExecAsyncEvent{
		EventType: rce.EventExec,
		Uid:       uid,
		Argv:      postReqest.Argv,
	})
	if err != nil {
		panic(err)
	}
	out, err := lib.LambdaClient().InvokeWithContext(ctx, &sdkLambda.InvokeInput{
		FunctionName:   aws.String(os.Getenv("AWS_LAMBDA_FUNCTION_NAME")),
		InvocationType: aws.String(sdkLambda.InvocationTypeEvent),
		LogType:        aws.String(sdkLambda.LogTypeNone),
		Payload:        data,
	})
	if err != nil {
		panic(err)
	}
	if *out.StatusCode != 202 {
		panic(out.StatusCode)
	}
	data, err = json.Marshal(rce.ExecPostResponse{
		Uid: uid,
	})
	if err != nil {
		panic(err)
	}
	res <- events.APIGatewayProxyResponse{
		StatusCode: 200,
		Body:       string(data),
		Headers:    corsHeaders,
	}
}

func handleApiEvent(ctx context.Context, event *events.APIGatewayProxyRequest, res chan<- events.APIGatewayProxyResponse) {
	if strings.HasPrefix(event.Path, "/js/main.js") ||
		strings.HasPrefix(event.Path, "/favicon.") {
		res <- static(event.Path)
		return
	}
	if strings.HasPrefix(event.Path, "/api/") {
		if event.HTTPMethod == http.MethodOptions {
			res <- events.APIGatewayProxyResponse{
				StatusCode: 200,
				Headers:    corsHeaders,
			}
			return
		}
		if !checkAuth(ctx, event.Headers["auth"]) {
			res <- unauthorized("bad auth")
			return
		}
		switch event.Path {
		case "/api/exec":
			switch event.HTTPMethod {
			case http.MethodGet:
				httpExecGet(ctx, event, res)
				return
			case http.MethodPost:
				httpExecPost(ctx, event, res)
				return
			default:
			}
		default:
		}
		res <- notfound()
		return
	}
	res <- index()
}

func atoi(x string) int {
	n, err := strconv.Atoi(x)
	if err != nil {
		panic(err)
	}
	return n
}

func unauthorized(body string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode: 401,
		Headers:    map[string]string{"Content-Type": ""},
		Body:       body,
	}
}

func logRecover(r interface{}, res chan<- events.APIGatewayProxyResponse) {
	stack := string(debug.Stack())
	lib.Logger.Println(r)
	lib.Logger.Println(stack)
	res <- events.APIGatewayProxyResponse{
		StatusCode: 500,
		Body:       fmt.Sprint(r) + "\n" + stack,
	}
}

func handleAsyncEvent(ctx context.Context, event *rce.ExecAsyncEvent) {
	bucket := os.Getenv("PROJECT_BUCKET")
	ctx, cancel := context.WithTimeout(ctx, 14*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, event.Argv[0], event.Argv[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		panic(err)
	}
	lines := make(chan *string, 128)
	for _, r := range []io.ReadCloser{stdout, stderr} {
		r := r
		go func() {
			readBuf := bufio.NewReader(r)
			for {
				line, err := readBuf.ReadString('\n')
				if err != nil {
					lines <- nil
					return
				}
				line = strings.TrimRight(line, "\n")
				lines <- &line
			}
		}()
	}
	logsDone := make(chan error)
	go func() {
		increment := 0
		doneCount := 0
		toShip := []string{}
		lastShipped := time.Now()
		shipLogsLock := sync.RWMutex{}
		shipLogs := func() {
			shipLogsLock.Lock()
			defer shipLogsLock.Unlock()
			val := strings.Join(toShip, "\n")
			val = strings.Trim(val, " \n")
			if val == "" {
				return
			}
			key := fmt.Sprintf("jobs/%s/logs.%05d", event.Uid, increment)
			err = lib.Retry(ctx, func() error {
				_, err := lib.S3Client().PutObjectWithContext(context.Background(), &s3.PutObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
					Body:   bytes.NewReader([]byte(val)),
				})
				return err
			})
			if err != nil {
				lib.Logger.Println("error:", err)
				return
			}
			toShip = nil
			increment++
			lastShipped = time.Now()
		}
		for line := range lines {
			if line == nil {
				doneCount++
				if doneCount == 2 {
					shipLogs()
					logsDone <- nil
					return
				}
				continue
			}
			toShip = append(toShip, *line)
			if time.Since(lastShipped) > 1*time.Second {
				shipLogs()
			}
		}
	}()
	err = cmd.Start()
	if err != nil {
	    panic(err)
	}
	<-logsDone
	exitCode := 0
	err = cmd.Wait()
	if err != nil {
		exitCode = 1
	}
	key := fmt.Sprintf("jobs/%s/exit", event.Uid)
	err = lib.Retry(ctx, func() error {
		_, err := lib.S3Client().PutObjectWithContext(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte(fmt.Sprint(exitCode))),
		})
		return err
	})
	if err != nil {
		panic(err)
	}
}

func handle(ctx context.Context, event map[string]interface{}, res chan<- events.APIGatewayProxyResponse) {
	defer func() {
		if r := recover(); r != nil {
			logRecover(r, res)
		}
	}()
	if event["event_type"] == rce.EventExec {
		asyncEvent := &rce.ExecAsyncEvent{}
		err := mapstructure.Decode(event, asyncEvent)
		if err != nil {
			panic(err)
		}
		handleAsyncEvent(ctx, asyncEvent)
		res <- ok()
		return
	}
	_, ok := event["path"]
	if !ok {
		res <- notfound()
		return
	}
	apiEvent := &events.APIGatewayProxyRequest{}
	err := mapstructure.Decode(event, apiEvent)
	if err != nil {
		panic(err)
	}
	handleApiEvent(ctx, apiEvent, res)
}

func handleRequest(ctx context.Context, event map[string]interface{}) (events.APIGatewayProxyResponse, error) {
	setupLogging(ctx)
	defer lib.Logger.Flush()
	start := time.Now()
	res := make(chan events.APIGatewayProxyResponse)
	go handle(ctx, event, res)
	r := <-res
	path, ok := event["path"]
	ts := time.Now().UTC().Format(time.RFC3339)
	if ok {
		lib.Logger.Println(r.StatusCode, path, time.Since(start), ts)
	} else {
		lib.Logger.Println(fmt.Sprintf("%#v", event), time.Since(start), ts)
	}
	return r, nil
}

func setupLogging(ctx context.Context) {
	lock := sync.RWMutex{}
	var lines []string
	uid := uuid.NewV4().String()
	count := 0
	lib.Logger = &lib.LoggerStruct{
		Print: func(args ...interface{}) {
			lock.Lock()
			defer lock.Unlock()
			lines = append(lines, fmt.Sprint(args...))
		},
		Flush: func() {
			lock.Lock()
			defer lock.Unlock()
			if len(lines) == 0 {
				return
			}
			text := strings.Join(lines, "")
			lines = nil
			unix := time.Now().Unix()
			key := fmt.Sprintf("logs/%d.%s.%03d", unix, uid, count)
			count++
			err := lib.Retry(context.Background(), func() error {
				_, err := lib.S3Client().PutObjectWithContext(context.Background(), &s3.PutObjectInput{
					Bucket: aws.String(os.Getenv("PROJECT_BUCKET")),
					Key:    aws.String(key),
					Body:   bytes.NewReader([]byte(text)),
				})
				return err
			})
			if err != nil {
				lib.Logger.Println("error:", err)
				return
			}
		},
	}
	go func() {
		for {
			lib.Logger.Flush()
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(5 * time.Second)
			}
		}
	}()
}

func main() {
	lambda.Start(handleRequest)
}
