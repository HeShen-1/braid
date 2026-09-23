package worker

import (
	"reflect"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	valid := DefaultConfig()
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"default", func(*Config) {}, false},
		{"concurrency zero", func(c *Config) { c.Concurrency = 0 }, true},
		{"concurrency over cap", func(c *Config) { c.Concurrency = 257 }, true},
		{"no queues", func(c *Config) { c.Queues = nil }, true},
		{"queue weight zero", func(c *Config) { c.Queues = map[string]int{"default": 0} }, true},
		{"queue name charset", func(c *Config) { c.Queues = map[string]int{"Bad!": 1} }, true},
		{"queue name length", func(c *Config) {
			c.Queues = map[string]int{"a12345678901234567890123456789012345678901234567890123456789012345": 1}
		}, true},
		{"worker id length", func(c *Config) { c.WorkerID = string(make([]byte, 65)) }, true},
		{"heartbeat too fast", func(c *Config) { c.HeartbeatInterval = time.Millisecond }, true},
		{"retry delay too fast", func(c *Config) { c.RetryDelay = time.Millisecond }, true},
		{"shutdown zero", func(c *Config) { c.ShutdownTimeout = 0 }, true},
		{"multi queue valid", func(c *Config) {
			c.Queues = map[string]int{"default": 1, "approval": 5}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "empty env returns defaults",
			env:  nil,
			want: DefaultConfig(),
		},
		{
			name: "full override",
			env: map[string]string{
				"BRAID_WORKER_CONCURRENCY":        "4",
				"BRAID_WORKER_QUEUES":             "default:1,approval:5",
				"BRAID_WORKER_ID":                 "worker-a",
				"BRAID_WORKER_HEARTBEAT_INTERVAL": "2s",
				"BRAID_WORKER_RETRY_DELAY":        "500ms",
				"BRAID_WORKER_SHUTDOWN_TIMEOUT":   "10s",
				"BRAID_WORKER_SNOWFLAKE_NODE":     "3",
			},
			want: Config{
				Concurrency:       4,
				Queues:            map[string]int{"default": 1, "approval": 5},
				WorkerID:          "worker-a",
				HeartbeatInterval: 2 * time.Second,
				RetryDelay:        500 * time.Millisecond,
				ShutdownTimeout:   10 * time.Second,
				SnowflakeNode:     3,
			},
		},
		{
			name:    "snowflake node over max",
			env:     map[string]string{"BRAID_WORKER_SNOWFLAKE_NODE": "1024"},
			wantErr: true,
		},
		{
			name:    "bad duration",
			env:     map[string]string{"BRAID_WORKER_RETRY_DELAY": "fast"},
			wantErr: true,
		},
		{
			name:    "bad int",
			env:     map[string]string{"BRAID_WORKER_CONCURRENCY": "many"},
			wantErr: true,
		},
		{
			name:    "queues entry without weight",
			env:     map[string]string{"BRAID_WORKER_QUEUES": "default"},
			wantErr: true,
		},
		{
			name:    "queues entry with bad weight",
			env:     map[string]string{"BRAID_WORKER_QUEUES": "default:high"},
			wantErr: true,
		},
		{
			name:    "validate catches propagated invalidity",
			env:     map[string]string{"BRAID_WORKER_CONCURRENCY": "0"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(k string) string { return tt.env[k] }
			got, err := ConfigFromEnv(getenv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ConfigFromEnv() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ConfigFromEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
