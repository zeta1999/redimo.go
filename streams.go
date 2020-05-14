package redimo

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type XID string

var ErrXGroupNotInitialized = errors.New("group not initialized")

const consumerKey = "cnk"
const lastDeliveryTimestampKey = "ldk"
const deliveryCountKey = "dck"
const (
	XStart  XID = "00000000000000000000-00000000000000000000"
	XEnd    XID = "99999999999999999999-99999999999999999999"
	XAutoID XID = "*"
)

func NewXID(ts time.Time, seq uint64) XID {
	timePart := fmt.Sprintf("%020d", ts.Unix())
	sequencePart := fmt.Sprintf("%020d", seq)

	return XID(strings.Join([]string{timePart, sequencePart}, "-"))
}

func (xid XID) String() string {
	return string(xid)
}

const sequenceSK = "_redimo/sequence"

func (xid XID) sequenceUpdateAction(key string, table string) dynamodb.TransactWriteItem {
	builder := newExpresionBuilder()
	builder.condition(fmt.Sprintf("#%v < :%v", vk, vk), vk)
	builder.SET(fmt.Sprintf("#%v = :%v", vk, vk), vk, StringValue{xid.String()}.toAV())

	return dynamodb.TransactWriteItem{
		Update: &dynamodb.Update{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       keyDef{pk: key, sk: sequenceSK}.toAV(),
			TableName:                 aws.String(table),
			UpdateExpression:          builder.updateExpression(),
		},
	}
}

func (xid XID) Next() XID {
	return NewXID(xid.Time(), xid.Seq()+1)
}

func (xid XID) Time() time.Time {
	parts := strings.Split(xid.String(), "-")
	tsec, _ := strconv.ParseInt(parts[0], 10, 64)

	return time.Unix(tsec, 0)
}

func (xid XID) Seq() uint64 {
	parts := strings.Split(xid.String(), "-")
	seq, _ := strconv.ParseUint(parts[1], 10, 64)

	return seq
}

type StreamItem struct {
	ID     XID
	Fields map[string]string
}

type PendingItem struct {
	ID            XID
	Consumer      string
	LastDelivered time.Time
	DeliveryCount int64
}

func (pi PendingItem) toPutAction(key string, table string) dynamodb.TransactWriteItem {
	builder := newExpresionBuilder()
	builder.updateSET(consumerKey, StringValue{pi.Consumer})
	builder.updateSET(lastDeliveryTimestampKey, NumericValue{big.NewFloat(float64(pi.LastDelivered.Unix()))})
	builder.clauses["ADD"] = append(builder.clauses["ADD"], fmt.Sprintf("#%v :delta", deliveryCountKey))
	builder.keys[deliveryCountKey] = struct{}{}
	builder.values["delta"] = NumericValue{big.NewFloat(1)}.toAV()

	return dynamodb.TransactWriteItem{
		Update: &dynamodb.Update{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       keyDef{pk: key, sk: pi.ID.String()}.toAV(),
			TableName:                 aws.String(table),
			UpdateExpression:          builder.updateExpression(),
		},
	}
}

func (pi PendingItem) updateDeliveryAction(key string, table string) *dynamodb.UpdateItemInput {
	builder := newExpresionBuilder()
	builder.addConditionEquality(consumerKey, StringValue{pi.Consumer})
	builder.updateSET(lastDeliveryTimestampKey, NumericValue{big.NewFloat(float64(time.Now().Unix()))})
	builder.clauses["ADD"] = append(builder.clauses["ADD"], fmt.Sprintf("#%v :delta", deliveryCountKey))
	builder.keys[deliveryCountKey] = struct{}{}
	builder.values["delta"] = NumericValue{big.NewFloat(1)}.toAV()

	return &dynamodb.UpdateItemInput{
		ConditionExpression:       builder.conditionExpression(),
		ExpressionAttributeNames:  builder.expressionAttributeNames(),
		ExpressionAttributeValues: builder.expressionAttributeValues(),
		Key:                       keyDef{pk: key, sk: pi.ID.String()}.toAV(),
		TableName:                 aws.String(table),
		UpdateExpression:          builder.updateExpression(),
	}
}

func parsePendingItem(avm map[string]dynamodb.AttributeValue) (pi PendingItem) {
	pi.ID = XID(aws.StringValue(avm[sk].S))
	pi.Consumer = aws.StringValue(avm[consumerKey].S)
	timestamp, _ := strconv.ParseInt(aws.StringValue(avm[lastDeliveryTimestampKey].N), 10, 64)
	pi.LastDelivered = time.Unix(timestamp, 0)
	deliveryCount, _ := strconv.ParseInt(aws.StringValue(avm[deliveryCountKey].N), 10, 64)
	pi.DeliveryCount = deliveryCount

	return
}

func (i StreamItem) putAction(key string, table string) dynamodb.TransactWriteItem {
	return dynamodb.TransactWriteItem{
		Put: &dynamodb.Put{
			Item:      i.toAV(key),
			TableName: aws.String(table),
		},
	}
}

func (i StreamItem) toAV(key string) map[string]dynamodb.AttributeValue {
	avm := make(map[string]dynamodb.AttributeValue)
	avm[pk] = StringValue{key}.toAV()
	avm[sk] = StringValue{i.ID.String()}.toAV()

	for k, v := range i.Fields {
		avm["_"+k] = StringValue{v}.toAV()
	}

	return avm
}

func (c Client) XACK(key string, group string, ids ...XID) (ackCount int64, err error) {
	for _, id := range ids {
		resp, err := c.ddbClient.DeleteItemRequest(&dynamodb.DeleteItemInput{
			Key:          keyDef{pk: c.xGroupKey(key, group), sk: id.String()}.toAV(),
			ReturnValues: dynamodb.ReturnValueAllOld,
			TableName:    aws.String(c.table),
		}).Send(context.TODO())
		if err != nil {
			return ackCount, err
		}

		if len(resp.Attributes) > 0 {
			ackCount++
		}
	}

	return
}

func (c Client) XADD(key string, id XID, fields map[string]string) (returnedID XID, err error) {
	retry := true
	retryCount := 0

	for retry && retryCount < 2 {
		var actions []dynamodb.TransactWriteItem

		if id == XAutoID {
			now := time.Now()
			newSequence, err := c.HINCRBY(key, "_redimo/sequence/"+fmt.Sprintf("%020d", now.Unix()), big.NewInt(1))

			if err != nil {
				return id, err
			}

			id = NewXID(now, newSequence.Uint64())
		}

		actions = append(actions, StreamItem{ID: id, Fields: fields}.putAction(key, c.table))
		actions = append(actions, id.sequenceUpdateAction(key, c.table))

		_, err := c.ddbClient.TransactWriteItemsRequest(&dynamodb.TransactWriteItemsInput{
			TransactItems: actions,
		}).Send(context.TODO())
		if err != nil {
			if conditionFailureError(err) && retryCount == 0 && c.xInit(key) == nil {
				// Steam may not have been initialized, but should be now
			} else {
				// err was an actual error
				return returnedID, err
			}
		} else {
			retry = false
		}
		retryCount++
	}

	return id, err
}

func (c Client) xInit(key string) (err error) {
	_, err = c.ddbClient.TransactWriteItemsRequest(&dynamodb.TransactWriteItemsInput{
		TransactItems: []dynamodb.TransactWriteItem{c.xInitAction(key)},
	}).Send(context.TODO())
	if conditionFailureError(err) {
		err = nil
	}

	return
}

func (c Client) xInitAction(key string) dynamodb.TransactWriteItem {
	builder := newExpresionBuilder()
	builder.addConditionNotExists(vk)
	builder.SET(fmt.Sprintf("#%v = :%v", vk, vk), vk, StringValue{XStart.String()}.toAV())

	return dynamodb.TransactWriteItem{
		Update: &dynamodb.Update{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       keyDef{pk: key, sk: sequenceSK}.toAV(),
			TableName:                 aws.String(c.table),
			UpdateExpression:          builder.updateExpression(),
		},
	}
}

func (c Client) XCLAIM(key string, group string, consumer string, lastDeliveredBefore time.Time, ids ...XID) (items []StreamItem, err error) {
	for _, id := range ids {
		builder := newExpresionBuilder()
		builder.addConditionExists(pk)
		builder.addConditionLessThanOrEqualTo(lastDeliveryTimestampKey, NumericValue{big.NewFloat(float64(lastDeliveredBefore.Unix()))})
		builder.updateSET(lastDeliveryTimestampKey, NumericValue{big.NewFloat(float64(time.Now().Unix()))})
		builder.updateSET(deliveryCountKey, NumericValue{big.NewFloat(0)})
		builder.updateSET(consumerKey, StringValue{consumer})

		_, err = c.ddbClient.UpdateItemRequest(&dynamodb.UpdateItemInput{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       keyDef{pk: c.xGroupKey(key, group), sk: id.String()}.toAV(),
			TableName:                 aws.String(c.table),
			UpdateExpression:          builder.updateExpression(),
		}).Send(context.TODO())

		if conditionFailureError(err) {
			continue
		}

		if err != nil {
			return items, err
		}

		fetchedItems, err := c.XRANGE(key, id, id, 1)

		if err != nil || len(fetchedItems) < 1 {
			return items, fmt.Errorf("could not loat stream item: %w", err)
		}

		items = append(items, fetchedItems[0])
	}

	return items, nil
}

func (c Client) XDEL(key string, ids ...XID) (count int64, err error) {
	for _, id := range ids {
		resp, err := c.ddbClient.DeleteItemRequest(&dynamodb.DeleteItemInput{
			Key:          keyDef{pk: key, sk: id.String()}.toAV(),
			ReturnValues: dynamodb.ReturnValueAllOld,
			TableName:    aws.String(c.table),
		}).Send(context.TODO())
		if err != nil {
			return count, err
		}

		if len(resp.Attributes) > 0 {
			count++
		}
	}

	return
}

func (c Client) XGROUP(key string, group string, start XID) (err error) {
	err = c.xGroupCursorSet(key, group, start)
	return
}

func (c Client) xGroupCursorSet(key string, group string, start XID) error {
	cursorKey := c.xGroupCursorKey(key, group)
	_, err := c.HSET(cursorKey.pk, map[string]Value{cursorKey.sk: StringValue{start.String()}})

	return err
}

func (c Client) xGroupCursorGet(key string, group string) (id XID, err error) {
	resp, err := c.ddbClient.GetItemRequest(&dynamodb.GetItemInput{
		ConsistentRead: aws.Bool(true),
		Key:            c.xGroupCursorKey(key, group).toAV(),
		TableName:      aws.String(c.table),
	}).Send(context.TODO())
	if err != nil {
		return
	}

	cursor := aws.StringValue(resp.Item[vk].S)
	if cursor == "" {
		return id, ErrXGroupNotInitialized
	}

	return XID(cursor), nil
}

func (c Client) xGroupCursorKey(key string, group string) keyDef {
	return keyDef{pk: c.xGroupKey(key, group), sk: "_redimo/cursor"}
}

func (c Client) xGroupKey(key string, group string) string {
	return strings.Join([]string{"_redimo", key, group}, "/")
}

func (c Client) XLEN(key string, start, stop XID) (count int64, err error) {
	hasMoreResults := true

	var cursor map[string]dynamodb.AttributeValue

	for hasMoreResults {
		builder := newExpresionBuilder()
		builder.addConditionEquality(pk, StringValue{key})
		builder.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", sk), sk)
		builder.values["start"] = dynamodb.AttributeValue{S: aws.String(start.String())}
		builder.values["stop"] = dynamodb.AttributeValue{S: aws.String(stop.String())}
		resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
			ConsistentRead:            aws.Bool(c.consistentReads),
			ExclusiveStartKey:         cursor,
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			KeyConditionExpression:    builder.conditionExpression(),
			ScanIndexForward:          aws.Bool(true),
			Select:                    dynamodb.SelectCount,
			TableName:                 aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return count, err
		}

		if len(resp.LastEvaluatedKey) > 0 {
			cursor = resp.LastEvaluatedKey
		} else {
			hasMoreResults = false
		}

		count += aws.Int64Value(resp.Count)
	}

	return
}

func (c Client) XPENDING(key string, group string, count int64) (pendingItems []PendingItem, err error) {
	hasMoreResults := true

	var cursor map[string]dynamodb.AttributeValue

	for hasMoreResults && count > 0 {
		builder := newExpresionBuilder()
		builder.addConditionEquality(pk, StringValue{c.xGroupKey(key, group)})
		builder.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", sk), sk)
		builder.values["start"] = StringValue{XStart.String()}.toAV()
		builder.values["stop"] = StringValue{XEnd.String()}.toAV()

		resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
			ConsistentRead:            aws.Bool(c.consistentReads),
			ExclusiveStartKey:         cursor,
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			KeyConditionExpression:    builder.conditionExpression(),
			Limit:                     aws.Int64(count),
			ScanIndexForward:          aws.Bool(true),
			TableName:                 aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return pendingItems, err
		}

		if len(resp.LastEvaluatedKey) > 0 {
			cursor = resp.LastEvaluatedKey
		} else {
			hasMoreResults = false
		}

		for _, item := range resp.Items {
			pendingItems = append(pendingItems, parsePendingItem(item))
			count--
		}
	}

	return
}

func (c Client) XRANGE(key string, start, stop XID, count int64) (streamItems []StreamItem, err error) {
	return c.xRange(key, start, stop, count, true)
}

func (c Client) xRange(key string, start, stop XID, count int64, forward bool) (streamItems []StreamItem, err error) {
	hasMoreResults := true

	var cursor map[string]dynamodb.AttributeValue

	for hasMoreResults && count > 0 {
		builder := newExpresionBuilder()
		builder.addConditionEquality(pk, StringValue{key})
		builder.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", sk), sk)
		builder.values["start"] = dynamodb.AttributeValue{S: aws.String(start.String())}
		builder.values["stop"] = dynamodb.AttributeValue{S: aws.String(stop.String())}
		resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
			ConsistentRead:            aws.Bool(c.consistentReads),
			ExclusiveStartKey:         cursor,
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			KeyConditionExpression:    builder.conditionExpression(),
			Limit:                     aws.Int64(count),
			ScanIndexForward:          aws.Bool(forward),
			TableName:                 aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return streamItems, err
		}

		if len(resp.LastEvaluatedKey) > 0 {
			cursor = resp.LastEvaluatedKey
		} else {
			hasMoreResults = false
		}

		for _, resultItem := range resp.Items {
			streamItems = append(streamItems, parseStreamItem(resultItem))
			count--
		}
	}

	return
}

func parseStreamItem(item map[string]dynamodb.AttributeValue) (si StreamItem) {
	si.Fields = make(map[string]string)

	for k, v := range item {
		if strings.HasPrefix(k, "_") {
			si.Fields[k[1:]] = aws.StringValue(v.S)
		}
	}

	si.ID = XID(aws.StringValue(item[sk].S))

	return
}

func (c Client) xGroupCursorPushAction(key string, group string, id XID) dynamodb.TransactWriteItem {
	builder := newExpresionBuilder()
	builder.updateSET(vk, StringValue{id.String()})
	builder.addConditionLessThan(vk, StringValue{id.String()})

	return dynamodb.TransactWriteItem{
		Update: &dynamodb.Update{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       c.xGroupCursorKey(key, group).toAV(),
			TableName:                 aws.String(c.table),
			UpdateExpression:          builder.updateExpression(),
		},
	}
}

func (c Client) XREAD(key string, from XID, count int64) (items []StreamItem, err error) {
	return c.XRANGE(key, from.Next(), XEnd, count)
}

type XReadOption string

const (
	XReadPending    XReadOption = "PENDING"
	XReadNew        XReadOption = "READ_NEW"
	XReadNewAutoACK XReadOption = "READ_NEW_NO_ACK"
)

func (c Client) xGroupReadPending(key string, group string, consumer string, count int64) (items []StreamItem, err error) {
	hasMoreResults := true

	var cursor map[string]dynamodb.AttributeValue

	for hasMoreResults && count > 0 {
		query := newExpresionBuilder()
		query.addConditionEquality(pk, StringValue{c.xGroupKey(key, group)})
		query.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", sk), sk)
		query.values["start"] = StringValue{XStart.String()}.toAV()
		query.values["stop"] = StringValue{XEnd.String()}.toAV()
		query.values[consumerKey] = StringValue{consumer}.toAV()
		query.keys[consumerKey] = struct{}{}
		resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
			ConsistentRead:            aws.Bool(c.consistentReads),
			ExclusiveStartKey:         cursor,
			ExpressionAttributeNames:  query.expressionAttributeNames(),
			ExpressionAttributeValues: query.expressionAttributeValues(),
			FilterExpression:          aws.String(fmt.Sprintf("#%v = :%v", consumerKey, consumerKey)),
			KeyConditionExpression:    query.conditionExpression(),
			Limit:                     aws.Int64(count),
			ScanIndexForward:          aws.Bool(true),
			TableName:                 aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return items, err
		}

		if len(resp.LastEvaluatedKey) > 0 {
			cursor = resp.LastEvaluatedKey
		} else {
			hasMoreResults = false
		}

		for _, item := range resp.Items {
			pendingItem := parsePendingItem(item)

			_, err = c.ddbClient.UpdateItemRequest(pendingItem.updateDeliveryAction(c.xGroupKey(key, group), c.table)).Send(context.TODO())
			if err != nil {
				return items, err
			}

			fetchedItems, err := c.XRANGE(key, pendingItem.ID, pendingItem.ID, 1)
			if err != nil || len(fetchedItems) < 1 {
				return items, err
			}

			items = append(items, fetchedItems[0])
			count--
		}
	}

	return
}

func (c Client) XREADGROUP(key string, group string, consumer string, option XReadOption, maxCount int64) (items []StreamItem, err error) {
	if option == XReadPending {
		return c.xGroupReadPending(key, group, consumer, maxCount)
	}

	retryCount := 0

	for retryCount < 5 {
		currentCursor, err := c.xGroupCursorGet(key, group)
		if err != nil {
			return items, err
		}

		items, err := c.XRANGE(key, currentCursor.Next(), XEnd, 1)

		if err != nil || len(items) == 0 {
			return items, err
		}

		item := items[0]

		var actions []dynamodb.TransactWriteItem
		actions = append(actions, c.xGroupCursorPushAction(key, group, item.ID))

		if option == XReadNew {
			actions = append(actions, PendingItem{
				ID:            item.ID,
				Consumer:      consumer,
				LastDelivered: time.Now(),
			}.toPutAction(c.xGroupKey(key, group), c.table))
		}

		_, err = c.ddbClient.TransactWriteItemsRequest(&dynamodb.TransactWriteItemsInput{
			TransactItems: actions,
		}).Send(context.TODO())
		if err == nil {
			return items, nil
		}

		if !conditionFailureError(err) {
			return items, err
		}
		retryCount++
	}

	return items, errors.New("too much contention")
}

func (c Client) XREVRANGE(key string, end, start XID, count int64) (streamItems []StreamItem, err error) {
	return c.xRange(key, start, end, count, false)
}

func (c Client) XTRIM(key string, newCount int64) (deletedCount int64, err error) {
	hasMoreResults := true

	var cursor map[string]dynamodb.AttributeValue

	for hasMoreResults {
		builder := newExpresionBuilder()
		builder.addConditionEquality(pk, StringValue{key})
		builder.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", sk), sk)
		builder.values["start"] = dynamodb.AttributeValue{S: aws.String(XStart.String())}
		builder.values["stop"] = dynamodb.AttributeValue{S: aws.String(XEnd.String())}
		resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
			ConsistentRead:            aws.Bool(c.consistentReads),
			ExclusiveStartKey:         cursor,
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			KeyConditionExpression:    builder.conditionExpression(),
			ProjectionExpression:      aws.String(strings.Join([]string{pk, sk}, ",")),
			ScanIndexForward:          aws.Bool(false),
			TableName:                 aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return deletedCount, err
		}

		if len(resp.LastEvaluatedKey) > 0 {
			cursor = resp.LastEvaluatedKey
		} else {
			hasMoreResults = false
		}

		var sortKeys []string

		for _, item := range resp.Items {
			if newCount == 0 {
				parsedItem := parseKey(item)
				sortKeys = append(sortKeys, parsedItem.sk)
			} else {
				newCount--
			}
		}

		if len(sortKeys) > 0 {
			deletedCount += int64(len(sortKeys))
			err = c.HDEL(key, sortKeys...)

			if err != nil {
				return deletedCount, err
			}
		}
	}

	return
}