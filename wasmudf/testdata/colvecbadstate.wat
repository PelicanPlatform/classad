;; colvecbadstate -- declares a result state the engine does not have.
;;
;; State 7 is past every state a value can be in. One byte describes the whole
;; column here, so one bad byte is the whole column being a value of no kind, and
;; the batch is refused rather than carrying it.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 7))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res)
      (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
    (i32.const 0))
)
