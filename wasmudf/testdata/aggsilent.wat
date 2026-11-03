;; aggsilent -- reports success from result without writing the value slot.
;;
;; The host poisons that slot before every call, so this is caught rather than
;; being credited with whatever the previous use of the instance left there. An
;; instance is pooled and reused, so without the poison the stale bytes would be
;; a PLAUSIBLE answer: the last aggregate finalized through it, returned for a
;; different query, with nothing anywhere reporting a problem.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param $sp i32) (param $sl i32) (param $vp i32) (param $n i32)
              (param $op i32) (param $oc i32) (result i32)
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $sp)) (i64.extend_i32_u (local.get $n))))
    (i32.const 8))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $ap)) (i64.load (local.get $bp))))
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
