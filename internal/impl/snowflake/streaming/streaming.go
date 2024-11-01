/*
 * Copyright 2024 Redpanda Data, Inc.
 *
 * Licensed as a Redpanda Enterprise file under the Redpanda Community
 * License (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 * https://github.com/redpanda-data/redpanda/blob/master/licenses/rcl.md
 */

package streaming

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rsa"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/parquet-go/parquet-go"
	"github.com/redpanda-data/benthos/v4/public/service"

	"github.com/redpanda-data/connect/v4/internal/periodic"
	"github.com/redpanda-data/connect/v4/internal/typed"
)

const debug = false

// ClientOptions is the options to create a Snowflake Snowpipe API Client
type ClientOptions struct {
	// Account name
	Account string
	// username
	User string
	// Snowflake Role (i.e. ACCOUNTADMIN)
	Role string
	// Private key for the user
	PrivateKey *rsa.PrivateKey
	// Logger for... logging?
	Logger         *service.Logger
	ConnectVersion string
	Application    string
}

type stageUploaderResult struct {
	uploader uploader
	err      error
}

// SnowflakeServiceClient is a port from Java :)
type SnowflakeServiceClient struct {
	client           *SnowflakeRestClient
	clientPrefix     string
	deploymentID     int64
	options          ClientOptions
	requestIDCounter *atomic.Int64

	uploader          *typed.AtomicValue[stageUploaderResult]
	uploadRefreshLoop *periodic.Periodic
}

// NewSnowflakeServiceClient creates a new API client for the Snowpipe Streaming API
func NewSnowflakeServiceClient(ctx context.Context, opts ClientOptions) (*SnowflakeServiceClient, error) {
	client, err := NewRestClient(
		opts.Account,
		opts.User,
		opts.ConnectVersion,
		opts.Application,
		opts.PrivateKey,
		opts.Logger,
	)
	if err != nil {
		return nil, err
	}
	resp, err := client.configureClient(ctx, clientConfigureRequest{Role: opts.Role})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != responseSuccess {
		return nil, fmt.Errorf("unable to initialize client - status: %d, message: %s", resp.StatusCode, resp.Message)
	}
	uploader, err := newUploader(resp.StageLocation)
	if err != nil {
		return nil, fmt.Errorf("unable to initialize stage uploader: %w", err)
	}
	uploaderAtomic := typed.NewAtomicValue(stageUploaderResult{
		uploader: uploader,
	})
	ssc := &SnowflakeServiceClient{
		client:       client,
		clientPrefix: fmt.Sprintf("%s_%d", resp.Prefix, resp.DeploymentID),
		deploymentID: resp.DeploymentID,
		options:      opts,

		uploader: uploaderAtomic,
		// Tokens expire every hour, so refresh a bit before that
		uploadRefreshLoop: periodic.NewWithContext(time.Hour-(2*time.Minute), func(ctx context.Context) {
			resp, err := client.configureClient(ctx, clientConfigureRequest{Role: opts.Role})
			if err != nil {
				uploaderAtomic.Store(stageUploaderResult{err: err})
				return
			}
			// TODO: Do the other checks here that the Java SDK does (deploymentID, etc)
			uploader, err := newUploader(resp.StageLocation)
			uploaderAtomic.Store(stageUploaderResult{uploader: uploader, err: err})
		}),
		requestIDCounter: &atomic.Int64{},
	}
	ssc.uploadRefreshLoop.Start()
	return ssc, nil
}

// Close closes the client and future requests have undefined behavior.
func (c *SnowflakeServiceClient) Close() error {
	c.uploadRefreshLoop.Stop()
	c.client.Close()
	return nil
}

func (c *SnowflakeServiceClient) nextRequestID() string {
	rid := c.requestIDCounter.Add(1)
	return fmt.Sprintf("%s_%d", c.clientPrefix, rid)
}

// ChannelOptions the parameters to opening a channel using SnowflakeServiceClient
type ChannelOptions struct {
	// ID of this channel, should be unique per channel
	ID int16
	// Name is the name of the channel
	Name string
	// DatabaseName is the name of the database
	DatabaseName string
	// SchemaName is the name of the schema
	SchemaName string
	// TableName is the name of the table
	TableName string
}

type encryptionInfo struct {
	encryptionKeyID int64
	encryptionKey   string
}

// OpenChannel creates a new or reuses a channel to load data into a Snowflake table.
func (c *SnowflakeServiceClient) OpenChannel(ctx context.Context, opts ChannelOptions) (*SnowflakeIngestionChannel, error) {
	resp, err := c.client.openChannel(ctx, openChannelRequest{
		RequestID: c.nextRequestID(),
		Role:      c.options.Role,
		Channel:   opts.Name,
		Database:  opts.DatabaseName,
		Schema:    opts.SchemaName,
		Table:     opts.TableName,
		WriteMode: "CLOUD_STORAGE",
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != responseSuccess {
		return nil, fmt.Errorf("unable to open channel %s - status: %d, message: %s", opts.Name, resp.StatusCode, resp.Message)
	}
	schema, transformers, typeMetadata, err := constructParquetSchema(resp.TableColumns)
	if err != nil {
		return nil, err
	}
	ch := &SnowflakeIngestionChannel{
		ChannelOptions: opts,
		clientPrefix:   c.clientPrefix,
		schema:         schema,
		version:        c.options.ConnectVersion,
		client:         c.client,
		role:           c.options.Role,
		uploader:       c.uploader,
		encryptionInfo: &encryptionInfo{
			encryptionKeyID: resp.EncryptionKeyID,
			encryptionKey:   resp.EncryptionKey,
		},
		clientSequencer:  resp.ClientSequencer,
		rowSequencer:     resp.RowSequencer,
		transformers:     transformers,
		fileMetadata:     typeMetadata,
		buffer:           bytes.NewBuffer(nil),
		requestIDCounter: c.requestIDCounter,
	}
	return ch, nil
}

// OffsetToken is the persisted client offset of a stream. This can be used to implement exactly-once
// processing.
type OffsetToken string

// ChannelStatus returns the offset token for a channel or an error
func (c *SnowflakeServiceClient) ChannelStatus(ctx context.Context, opts ChannelOptions) (OffsetToken, error) {
	resp, err := c.client.channelStatus(ctx, batchChannelStatusRequest{
		Role: c.options.Role,
		Channels: []channelStatusRequest{
			{
				Name:     opts.Name,
				Table:    opts.TableName,
				Database: opts.DatabaseName,
				Schema:   opts.SchemaName,
			},
		},
	})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != responseSuccess {
		return "", fmt.Errorf("unable to status channel %s - status: %d, message: %s", opts.Name, resp.StatusCode, resp.Message)
	}
	if len(resp.Channels) != 1 {
		return "", fmt.Errorf("failed to fetch channel %s, got %d channels in response", opts.Name, len(resp.Channels))
	}
	channel := resp.Channels[0]
	if channel.StatusCode != responseSuccess {
		return "", fmt.Errorf("unable to status channel %s - status: %d", opts.Name, resp.StatusCode)
	}
	return OffsetToken(channel.PersistedOffsetToken), nil
}

// DropChannel drops it like it's hot 🔥
func (c *SnowflakeServiceClient) DropChannel(ctx context.Context, opts ChannelOptions) error {
	resp, err := c.client.dropChannel(ctx, dropChannelRequest{
		RequestID: c.nextRequestID(),
		Role:      c.options.Role,
		Channel:   opts.Name,
		Table:     opts.TableName,
		Database:  opts.DatabaseName,
		Schema:    opts.SchemaName,
	})
	if err != nil {
		return err
	}
	if resp.StatusCode != responseSuccess {
		return fmt.Errorf("unable to drop channel %s - status: %d, message: %s", opts.Name, resp.StatusCode, resp.Message)
	}
	return nil
}

// SnowflakeIngestionChannel is a write connection to a single table in Snowflake
type SnowflakeIngestionChannel struct {
	ChannelOptions
	role            string
	clientPrefix    string
	version         string
	schema          *parquet.Schema
	client          *SnowflakeRestClient
	uploader        *typed.AtomicValue[stageUploaderResult]
	encryptionInfo  *encryptionInfo
	clientSequencer int64
	rowSequencer    int64
	transformers    []*dataTransformer
	fileMetadata    map[string]string
	buffer          *bytes.Buffer
	// This is shared among the various open channels to get some uniqueness
	// when naming bdec files
	requestIDCounter *atomic.Int64
}

func (c *SnowflakeIngestionChannel) nextRequestID() string {
	rid := c.requestIDCounter.Add(1)
	return fmt.Sprintf("%s_%d", c.clientPrefix, rid)
}

// InsertStats holds some basic statistics about the InsertRows operation
type InsertStats struct {
	BuildTime            time.Duration
	UploadTime           time.Duration
	CompressedOutputSize int
}

// InsertRows creates a parquet file using the schema from the data,
// then writes that file into the Snowflake table
func (c *SnowflakeIngestionChannel) InsertRows(ctx context.Context, batch service.MessageBatch) (InsertStats, error) {
	stats := InsertStats{}
	startTime := time.Now()
	rows, err := constructRowGroup(batch, c.schema, c.transformers)
	if err != nil {
		return stats, err
	}
	// Prevent multiple channels from having the same bdec file (it must be unique)
	// so add the ID of the channel in the upper 16 bits and then get 48 bits of
	// randomness outside that.
	fakeThreadID := (int(c.ID) << 48) | rand.N(1<<48)
	blobPath := generateBlobPath(c.clientPrefix, fakeThreadID, int(c.requestIDCounter.Add(1)))
	// This is extra metadata that is required for functionality in snowflake.
	c.fileMetadata["primaryFileId"] = path.Base(blobPath)
	c.buffer.Reset()
	err = writeParquetFile(c.buffer, c.version, parquetFileData{
		schema:   c.schema,
		rows:     rows,
		metadata: c.fileMetadata,
	})
	if err != nil {
		return stats, err
	}
	unencrypted := c.buffer.Bytes()
	metadata, err := readParquetMetadata(unencrypted)
	if err != nil {
		return stats, fmt.Errorf("unable to parse parquet metadata: %w", err)
	}
	if debug {
		_ = os.WriteFile("latest_test.parquet", unencrypted, 0o644)
	}
	unencryptedLen := len(unencrypted)
	unencrypted = padBuffer(unencrypted, aes.BlockSize)
	encrypted, err := encrypt(unencrypted, c.encryptionInfo.encryptionKey, blobPath, 0)
	if err != nil {
		return stats, err
	}
	uploadStartTime := time.Now()
	fileMD5Hash := md5.Sum(encrypted)
	uploaderResult := c.uploader.Load()
	if uploaderResult.err != nil {
		return stats, fmt.Errorf("failed to acquire stage uploader: %w", uploaderResult.err)
	}
	uploader := uploaderResult.uploader
	err = backoff.Retry(func() error {
		return uploader.upload(ctx, blobPath, encrypted, fileMD5Hash[:])
	}, backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Second), 3))
	if err != nil {
		return stats, err
	}

	uploadFinishTime := time.Now()
	columnEpInfo := computeColumnEpInfo(c.transformers)
	resp, err := c.client.registerBlob(ctx, registerBlobRequest{
		RequestID: c.nextRequestID(),
		Role:      c.role,
		Blobs: []blobMetadata{
			{
				Path:        blobPath,
				MD5:         hex.EncodeToString(fileMD5Hash[:]),
				BDECVersion: 3,
				BlobStats: blobStats{
					FlushStartMs:     startTime.UnixMilli(),
					BuildDurationMs:  uploadStartTime.UnixMilli() - startTime.UnixMilli(),
					UploadDurationMs: uploadFinishTime.UnixMilli() - uploadStartTime.UnixMilli(),
				},
				Chunks: []chunkMetadata{
					{
						Database:                c.DatabaseName,
						Schema:                  c.SchemaName,
						Table:                   c.TableName,
						ChunkStartOffset:        0,
						ChunkLength:             int32(unencryptedLen),
						ChunkLengthUncompressed: totalUncompressedSize(metadata),
						ChunkMD5:                md5Hash(encrypted[:unencryptedLen]),
						EncryptionKeyID:         c.encryptionInfo.encryptionKeyID,
						FirstInsertTimeInMillis: startTime.UnixMilli(),
						LastInsertTimeInMillis:  startTime.UnixMilli(),
						EPS: &epInfo{
							Rows:    metadata.NumRows,
							Columns: columnEpInfo,
						},
						Channels: []channelMetadata{
							{
								Channel:          c.Name,
								ClientSequencer:  c.clientSequencer,
								RowSequencer:     c.rowSequencer + 1,
								StartOffsetToken: nil,
								EndOffsetToken:   nil,
								OffsetToken:      nil,
							},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return stats, err
	}
	if len(resp.Blobs) != 1 {
		return stats, fmt.Errorf("unexpected number of response blobs: %d", len(resp.Blobs))
	}
	status := resp.Blobs[0]
	if len(status.Chunks) != 1 {
		return stats, fmt.Errorf("unexpected number of response blob chunks: %d", len(status.Chunks))
	}
	chunk := status.Chunks[0]
	if len(chunk.Channels) != 1 {
		return stats, fmt.Errorf("unexpected number of channels for blob chunk: %d", len(chunk.Channels))
	}
	channel := chunk.Channels[0]
	if channel.StatusCode != responseSuccess {
		msg := channel.Message
		if msg == "" {
			msg = "(no message)"
		}
		return stats, fmt.Errorf("error response injesting data (%d): %s", channel.StatusCode, msg)
	}
	c.rowSequencer++
	c.clientSequencer = channel.ClientSequencer
	stats.CompressedOutputSize = unencryptedLen
	stats.BuildTime = uploadStartTime.Sub(startTime)
	stats.UploadTime = uploadFinishTime.Sub(uploadStartTime)
	return stats, err
}

// WaitUntilCommitted waits until all the data in the channel has been committed
// along with how many polls it took to get that.
func (c *SnowflakeIngestionChannel) WaitUntilCommitted(ctx context.Context) (int, error) {
	var polls int
	err := backoff.Retry(func() error {
		polls++
		resp, err := c.client.channelStatus(ctx, batchChannelStatusRequest{
			Role: c.role,
			Channels: []channelStatusRequest{
				{
					Table:           c.TableName,
					Database:        c.DatabaseName,
					Schema:          c.SchemaName,
					Name:            c.Name,
					ClientSequencer: &c.clientSequencer,
				},
			},
		})
		if err != nil {
			return err
		}
		if resp.StatusCode != responseSuccess {
			msg := resp.Message
			if msg == "" {
				msg = "(no message)"
			}
			return fmt.Errorf("error fetching channel status (%d): %s", resp.StatusCode, msg)
		}
		if len(resp.Channels) != 1 {
			return fmt.Errorf("unexpected number of channels for status request: %d", len(resp.Channels))
		}
		status := resp.Channels[0]
		if status.PersistedClientSequencer != c.clientSequencer {
			return backoff.Permanent(fmt.Errorf("unexpected number of channels for status request: %d", len(resp.Channels)))
		}
		if status.PersistedRowSequencer < c.rowSequencer {
			return fmt.Errorf("row sequencer not yet committed: %d < %d", status.PersistedRowSequencer, c.rowSequencer)
		}
		return nil
	}, backoff.WithContext(
		// 1, 10, 100, 1000, 1000, ...
		backoff.NewExponentialBackOff(
			backoff.WithInitialInterval(time.Millisecond),
			backoff.WithMultiplier(10),
			backoff.WithMaxInterval(time.Second),
			backoff.WithMaxElapsedTime(10*time.Minute),
		),
		ctx,
	))
	return polls, err
}
