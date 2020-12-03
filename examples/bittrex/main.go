package main

import (
	"context"
	"log"

	"github.com/rainhq/signalr/v2"
	"golang.org/x/sync/errgroup"
)

// For more extensive use cases and capabilities, please see
// https://github.com/carterjones/bittrex.

func main() {
	ctx := context.Background()

	// Prepare a SignalR client.
	c, err := signalr.Dial(
		ctx,
		"https://socket.bittrex.com/signalr",
		`[{"name":"c2"}]`,
	)
	if err != nil {
		log.Fatal(err)
	}

	errg, ctx := errgroup.WithContext(ctx)

	errg.Go(func() error {
		var msg signalr.Message
		for {
			if err := c.ReadMessage(ctx, &msg); err != nil {
				return err
			}

			log.Println(msg)
		}
	})
	errg.Go(func() error {
		// Subscribe to the USDT-BTC feed.
		return c.WriteMessage(signalr.ClientMsg{
			H: "corehub",
			M: "SubscribeToExchangeDeltas",
			A: []interface{}{"USDT-BTC"},
			I: 1,
		})
	})

	if err := errg.Wait(); err != nil {
		log.Fatal(err)
	}
}
