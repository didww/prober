package agent

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

type pbTimestamp = timestamppb.Timestamp

func newTimestamp(t time.Time) *pbTimestamp { return timestamppb.New(t) }
