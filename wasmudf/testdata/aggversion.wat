;; aggversion -- reports an aggregate ABI version this host does not speak.
;;
;; Checked once at load and not per call, because a guest reading a layout the
;; host no longer writes does not fail: it computes, on whatever happens to be at
;; those offsets, and returns a number.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 99))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param i32) (param i32) (param i32) (param i32)
              (param i32) (param i32) (result i32)
    (i32.const 8))

  (func $merge (param i32) (param i32) (param i32) (param i32)
               (param i32) (param i32) (result i32)
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
