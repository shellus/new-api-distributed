package common

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJsonRawMessageToString(t *testing.T) {
	tests := []struct {
		name string
		data json.RawMessage
		want string
	}{
		{
			name: "object",
			data: json.RawMessage(`{"city":"Paris","days":0,"strict":false}`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "string",
			data: json.RawMessage(`"{\"city\":\"Paris\",\"days\":0,\"strict\":false}"`),
			want: `{"city":"Paris","days":0,"strict":false}`,
		},
		{
			name: "null",
			data: json.RawMessage(`null`),
			want: "",
		},
		{
			name: "empty",
			data: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, JsonRawMessageToString(tt.data))
		})
	}
}

func TestDecodeJsonStrictRejectsUnknownAndTrailingValues(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}

	var decoded payload
	require.NoError(t, DecodeJsonStrict(bytes.NewBufferString(`{"name":"edge"}`), &decoded))
	require.Equal(t, "edge", decoded.Name)

	require.Error(t, DecodeJsonStrict(bytes.NewBufferString(`{"name":"edge","secret":"no"}`), &decoded))
	require.Error(t, DecodeJsonStrict(bytes.NewBufferString(`{"name":"edge"} {"name":"second"}`), &decoded))
}
