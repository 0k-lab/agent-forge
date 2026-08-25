package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"agent-forge/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	gate := flag.String("gate", "", "Gate WebSocket base URL")
	id := flag.String("id", "worker-1", "worker ID")
	token := flag.String("token", "", "worker token")
	mode := flag.String("mode", "abandon", "abandon or late")
	job := flag.String("job", "", "late job ID")
	attempt := flag.String("attempt", "", "late attempt ID")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := http.Header{"Authorization": []string{"Bearer " + *token}}
	c, _, err := websocket.Dial(ctx, *gate+"/v1/workers/connect?worker_id="+*id, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		fail(err)
	}
	defer c.CloseNow()
	if *mode == "late" {
		err = wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageResult, JobID: *job, AttemptID: *attempt, Result: "late"})
		var reply protocol.Message
		if err == nil {
			err = wsjson.Read(ctx, c, &reply)
		}
		if err != nil || reply.Type != protocol.MessageError || reply.Error != "request failed" {
			fail(fmt.Errorf("late reply: %#v: %v", reply, err))
		}
		fmt.Println("late_rejected")
		return
	}
	var lease protocol.Message
	if err := wsjson.Read(ctx, c, &lease); err != nil {
		fail(err)
	}
	if lease.Type != protocol.MessageLease {
		fail(fmt.Errorf("unexpected message: %#v", lease))
	}
	if err := json.NewEncoder(os.Stdout).Encode(lease); err != nil {
		fail(err)
	}
	var ignored protocol.Message
	_ = wsjson.Read(ctx, c, &ignored)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
