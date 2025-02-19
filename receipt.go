// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package whatsmeow

import (
	"encoding/binary"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"go.mau.fi/libsignal/ecc"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func (cli *Client) handleReceipt(node *waBinary.Node) {
	receipt, err := cli.parseReceipt(node)
	if err != nil {
		cli.Log.Warnf("Failed to parse receipt: %v", err)
	} else {
		go cli.dispatchEvent(receipt)
	}
	go cli.sendAck(node)
}

func (cli *Client) parseReceipt(node *waBinary.Node) (*events.Receipt, error) {
	ag := node.AttrGetter()
	source, err := cli.parseMessageSource(node)
	if err != nil {
		return nil, err
	}
	receipt := events.Receipt{
		MessageSource: source,
		Timestamp:     time.Unix(ag.Int64("t"), 0),
		Type:          events.ReceiptType(ag.OptionalString("type")),
	}
	receipt.MessageID = ag.String("id")
	if !ag.OK() {
		return nil, fmt.Errorf("failed to parse read receipt attrs: %+v", ag.Errors)
	}

	receiptChildren := node.GetChildren()
	if len(receiptChildren) == 1 && receiptChildren[0].Tag == "list" {
		listChildren := receiptChildren[0].GetChildren()
		receipt.PreviousIDs = make([]string, 0, len(listChildren))
		for _, item := range listChildren {
			if id, ok := item.Attrs["id"].(string); ok && item.Tag == "item" {
				receipt.PreviousIDs = append(receipt.PreviousIDs, id)
			}
		}
	}
	return &receipt, nil
}

func (cli *Client) sendAck(node *waBinary.Node) {
	attrs := waBinary.Attrs{
		"class": node.Tag,
		"id":    node.Attrs["id"],
	}
	attrs["to"] = node.Attrs["from"]
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if receiptType, ok := node.Attrs["type"]; node.Tag != "message" && ok {
		attrs["type"] = receiptType
	}
	err := cli.sendNode(waBinary.Node{
		Tag:   "ack",
		Attrs: attrs,
	})
	if err != nil {
		cli.Log.Warnf("Failed to send acknowledgement for %s %s: %v", node.Tag, node.Attrs["id"], err)
	}
}

func (cli *Client) sendMessageReceipt(info *types.MessageInfo) {
	attrs := waBinary.Attrs{
		"id": info.ID,
	}
	if info.IsFromMe {
		attrs["type"] = "sender"
	} else {
		attrs["type"] = "inactive"
	}
	attrs["to"] = info.Chat
	if info.IsGroup {
		attrs["participant"] = info.Sender
	} else if info.IsFromMe {
		attrs["recipient"] = info.Sender
	}
	err := cli.sendNode(waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
	})
	if err != nil {
		cli.Log.Warnf("Failed to send receipt for %s: %v", info.ID, err)
	}
}

func (cli *Client) sendRetryReceipt(node *waBinary.Node, forceIncludeIdentity bool) {
	id, _ := node.Attrs["id"].(string)

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	cli.messageRetriesLock.Unlock()

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint32(registrationIDBytes[:], cli.Store.RegistrationID)
	attrs := waBinary.Attrs{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: waBinary.Attrs{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 || forceIncludeIdentity {
		if key, err := cli.Store.PreKeys.GenOnePreKey(); err != nil {
			cli.Log.Errorf("Failed to get prekey for retry receipt: %v", err)
		} else if deviceIdentity, err := proto.Marshal(cli.Store.Account); err != nil {
			cli.Log.Errorf("Failed to marshal account info: %v", err)
			return
		} else {
			payload.Content = append(payload.GetChildren(), waBinary.Node{
				Tag: "keys",
				Content: []waBinary.Node{
					{Tag: "type", Content: []byte{ecc.DjbType}},
					{Tag: "identity", Content: cli.Store.IdentityKey.Pub[:]},
					preKeyToNode(key),
					preKeyToNode(cli.Store.SignedPreKey),
					{Tag: "device-identity", Content: deviceIdentity},
				},
			})
		}
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorf("Failed to send retry receipt for %s: %v", id, err)
	}
}
