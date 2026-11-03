;; aggbadsig -- merge takes four parameters where the ABI says six.
;;
;; The signature is in the compiled module, so there is no reason to wait for a
;; call to find out: a mismatch discovered at query time is one failed query per
;; caller with nothing naming the cause, where this is a load error naming the
;; function and both signatures.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param i32) (param i32) (param i32) (param i32)
              (param i32) (param i32) (result i32)
    (i32.const 8))

  (func $merge (param i32) (param i32) (param i32) (param i32) (result i32)
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
