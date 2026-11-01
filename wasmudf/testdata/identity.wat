;; identity -- copies the one argument slot to the result slot verbatim. It is the
;; benchmark's guest and the round-trip fixture: whatever the host encodes, it gets
;; back, so a test that it returns the same value tests the ENCODING and not the
;; guest's arithmetic.
;;
;; A string's payload is a pointer into the host's own arena, so copying the slot
;; echoes the bytes without touching them -- which is exactly what makes this the
;; cheapest possible guest, and so the floor for per-call overhead.
(module
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (memory.copy (local.get $res) (local.get $args) (i32.const 16))
    (i32.const 0))
)
