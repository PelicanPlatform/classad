;; sentinel -- holds 32 bytes of known data at offset 0 and returns them.
;;
;; It exists to make the host's region placement observable. The host puts the
;; call frame in pages it ADDS with memory.grow, which cannot overlap a data
;; segment; a host that instead wrote the frame at a low address would scribble
;; the first 48 bytes of this module's data with a tag, a length and a pointer,
;; and the string would come back corrupted. Every other fixture here has an
;; empty data section, so without this one that mistake is invisible.
(module
  (memory (export "memory") 1)
  (data (i32.const 0) "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345")
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 5))
    (i32.store offset=4 (local.get $res) (i32.const 32))
    (i64.store offset=8 (local.get $res) (i64.const 0))
    (i32.const 0))
)
