;; hostimport -- asks for a WASI import. The host offers exactly one import and no
;; WASI at all, so this module cannot be instantiated: that is how "a UDF cannot do
;; I/O" is enforced, by there being nothing to call, and the load error names the
;; import it wanted.
(module
  (import "wasi_snapshot_preview1" "fd_write"
    (func $fd_write (param i32 i32 i32 i32) (result i32)))
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call") (param i32 i32 i32) (result i32)
    (call $fd_write (i32.const 1) (i32.const 0) (i32.const 0) (i32.const 0)))
)
