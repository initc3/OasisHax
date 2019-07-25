// Package syncer provides the read-only sync interface.
package syncer

import (
	"context"
	"errors"

	"github.com/oasislabs/ekiden/go/storage/mkvs/urkel/node"
)

var (
	ErrDirtyRoot    = errors.New("urkel: root is dirty")
	ErrInvalidRoot  = errors.New("urkel: invalid root")
	ErrNodeNotFound = errors.New("urkel: node not found during sync")
	ErrUnsupported  = errors.New("urkel: method not supported")
)

// PathOptions are the options for GetPath queries.
type PathOptions struct {
	// IncludeSiblings specifies whether the subtree should also include
	// off-path siblings. This is useful when the caller knows that it
	// will also need full sibling information during the local operation.
	IncludeSiblings bool `codec:"include_siblings"`
}

// ReadSyncer is the interface for synchronizing the in-memory cache
// with another (potentially untrusted) MKVS.
type ReadSyncer interface {
	// GetSubtree retrieves a compressed subtree summary of the given node
	// under the given root up to the specified depth.
	//
	// It is the responsibility of the caller to validate that the subtree
	// is correct and consistent.
	GetSubtree(ctx context.Context, root node.Root, id node.ID, maxDepth node.Depth) (*Subtree, error)

	// GetPath retrieves a compressed path summary for the given key under
	// the given root, starting at the given depth.
	//
	// It is the responsibility of the caller to validate that the subtree
	// is correct and consistent.
	GetPath(ctx context.Context, root node.Root, key node.Key, startBitDepth node.Depth) (*Subtree, error)

	// GetNode retrieves a specific node under the given root.
	//
	// It is the responsibility of the caller to validate that the node
	// is consistent. The node's cached hash should be considered invalid
	// and must be recomputed locally.
	GetNode(ctx context.Context, root node.Root, id node.ID) (node.Node, error)
}

// nopReadSyncer is a no-op read syncer.
type nopReadSyncer struct{}

// NewNopReadSyncer creates a new no-op read syncer.
func NewNopReadSyncer() ReadSyncer {
	return &nopReadSyncer{}
}

func (r *nopReadSyncer) GetSubtree(ctx context.Context, root node.Root, id node.ID, maxDepth node.Depth) (*Subtree, error) {
	return nil, ErrUnsupported
}

func (r *nopReadSyncer) GetPath(ctx context.Context, root node.Root, key node.Key, startDepth node.Depth) (*Subtree, error) {
	return nil, ErrNodeNotFound
}

func (r *nopReadSyncer) GetNode(ctx context.Context, root node.Root, id node.ID) (node.Node, error) {
	return nil, ErrNodeNotFound
}
