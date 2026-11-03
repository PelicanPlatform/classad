;; aggnoresult -- writes a perfectly good value into the result slot and then
;; returns a nonzero status to say it could not produce one.
;;
;; The slot and the status disagree on purpose. A host that read the slot because
;; it looked like a value would return 42 for every group of every query using
;; this module, and 42 is not a wrong-looking number. The status is what says
;; whether the slot means anything, and it is the only thing that does.
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
    (i32.store (local.get $rp) (i32.const 3))
    (i32.store offset=4 (local.get $rp) (i32.const 0))
    (i64.store offset=8 (local.get $rp) (i64.const 42))
    (i32.const 7))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
