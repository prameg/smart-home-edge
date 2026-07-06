// Command mqttobserve is a throwaway end-to-end debugging aid: it connects to a
// broker and prints every message under a topic filter with timestamps, so we
// can watch the agent<->cloud contract on the wire (the mosquitto 2.1.x CLI
// clients are broken on this macOS host). Optionally publishes one message.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

func main() {
	broker := flag.String("broker", "tcp://127.0.0.1:1883", "broker URL")
	filter := flag.String("t", "homes/#", "subscribe topic filter")
	pubTopic := flag.String("pub", "", "publish to this topic then keep listening")
	pubPayload := flag.String("m", "", "publish payload")
	pubRetain := flag.Bool("r", false, "publish retained")
	clientID := flag.String("id", "mqttobserve", "client id")
	flag.Parse()

	opts := paho.NewClientOptions().
		AddBroker(*broker).
		SetClientID(*clientID).
		SetCleanSession(true).
		SetConnectTimeout(10 * time.Second)

	client := paho.NewClient(opts)
	if t := client.Connect(); t.WaitTimeout(10*time.Second) && t.Error() != nil {
		fmt.Fprintln(os.Stderr, "connect error:", t.Error())
		os.Exit(1)
	}
	fmt.Printf("[connected] %s  filter=%s\n", *broker, *filter)

	if t := client.Subscribe(*filter, 1, func(_ paho.Client, m paho.Message) {
		fmt.Printf("%s  %s%s  %s\n",
			time.Now().Format("15:04:05.000"),
			m.Topic(),
			retainedTag(m.Retained()),
			string(m.Payload()),
		)
	}); t.Wait() && t.Error() != nil {
		fmt.Fprintln(os.Stderr, "subscribe error:", t.Error())
		os.Exit(1)
	}

	if *pubTopic != "" {
		if t := client.Publish(*pubTopic, 1, *pubRetain, *pubPayload); t.WaitTimeout(5*time.Second) && t.Error() != nil {
			fmt.Fprintln(os.Stderr, "publish error:", t.Error())
		} else {
			fmt.Printf("[published] %s retain=%v %s\n", *pubTopic, *pubRetain, *pubPayload)
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	client.Disconnect(200)
}

func retainedTag(retained bool) string {
	if retained {
		return " [retained]"
	}

	return ""
}
