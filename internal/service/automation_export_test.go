package service

import "context"

// Test-only access to the follower.
//
// The follower normally runs on a ticker inside Run. A test needs to step it
// deterministically -- submit, then advance, then advance again -- so it can
// assert what the engine asked for at each stage rather than racing a timer.
//
// Exported here rather than by making `follow` public, so the only caller
// outside this package is a test in this module and the production surface is
// unchanged.
func FollowForTest(s *AutomationService, ctx context.Context) { s.follow(ctx) }
