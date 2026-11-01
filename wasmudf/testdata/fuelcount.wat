;; fuelcount -- spends a bounded, declared amount of fuel and then returns a
;; value. It is the other half of the step-budget contract: a budget that only
;; ever fires is indistinguishable from a budget of zero, so one fixture must
;; spend fuel and SUCCEED.
;;
;; The loop runs 100 iterations declaring one step each, so a budget of 100 is
;; exactly enough and 99 is exactly one short -- a boundary, not a guess.
(module
  (import "classad_udf" "fuel" (func $fuel (param i32)))
  (memory (export "memory") 1)
  (func (export "classad_udf_abi_version") (result i32) (i32.const 1))
  (func (export "classad_udf_call")
        (param $args i32) (param $argc i32) (param $res i32) (result i32)
    (local $i i32)
    (loop $spend
      (call $fuel (i32.const 1))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $spend (i32.lt_u (local.get $i) (i32.const 100))))
    (i32.store (local.get $res) (i32.const 3))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.extend_i32_u (local.get $i)))
    (i32.const 0))
)
