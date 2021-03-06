package component

import (
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/vmware-tanzu/octant/pkg/action"

	"github.com/vmware-tanzu/octant/internal/util/json"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_Button_Marshal(t *testing.T) {
	tests := []struct {
		name         string
		input        func() *Button
		expectedFile string
		isErr        bool
	}{
		{
			name: "empty button",
			input: func() *Button {
				return NewButton("test", action.Payload{"foo": "bar"})
			},
			expectedFile: "button.json",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.input())
			if test.isErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			expected, err := ioutil.ReadFile(filepath.Join("testdata", test.expectedFile))
			require.NoError(t, err)

			assert.JSONEq(t, string(expected), string(got))

		})
	}
}
