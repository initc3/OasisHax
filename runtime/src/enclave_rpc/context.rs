//! RPC call context.
use std::{any::Any, sync::Arc};

use io_context::Context as IoContext;

use super::session::SessionInfo;
use crate::{consensus::verifier::Verifier, identity::Identity, storage::KeyValue};

struct NoRuntimeContext;

/// RPC call context.
pub struct Context<'a> {
    /// I/O context.
    pub io_ctx: Arc<IoContext>,
    /// The current runtime identity if any.
    pub identity: Arc<Identity>,
    /// Information about the session the RPC call was delivered over.
    pub session_info: Option<Arc<SessionInfo>>,
    /// Consensus verifier.
    pub consensus_verifier: Arc<dyn Verifier>,
    /// Runtime-specific context.
    pub runtime: Box<dyn Any>,
    /// Untrusted local storage.
    pub untrusted_local_storage: &'a dyn KeyValue,
}

impl<'a> Context<'a> {
    /// Construct new transaction context.
    pub fn new(
        io_ctx: Arc<IoContext>,
        identity: Arc<Identity>,
        session_info: Option<Arc<SessionInfo>>,
        consensus_verifier: Arc<dyn Verifier>,
        untrusted_local_storage: &'a dyn KeyValue,
    ) -> Self {
        Self {
            io_ctx,
            identity,
            session_info,
            consensus_verifier,
            runtime: Box::new(NoRuntimeContext),
            untrusted_local_storage,
        }
    }
}
