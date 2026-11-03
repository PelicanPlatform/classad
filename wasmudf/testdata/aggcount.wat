;; aggcount -- counts every value it is handed, undefined and error included, and
;; does NOT export the numeric fold.
;;
;; It is the fixture for the opt-in being a real gate rather than a formality.
;; Counting what it was offered is precisely the property the columnar pass
;; breaks: that pass yields only the column's numeric values, so a record whose
;; argument is absent or a string is never offered at all, and this module would
;; return a smaller number on the column rung than on the record scan -- the same
;; query, two answers, neither looking wrong. Withholding the export is how it
;; says so, and the engine must then serve it from the scan.
;;
;; State is one i64.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (if (i32.lt_u (local.get $cap) (i32.const 8))
      (then (return (i32.const -1))))
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param $sp i32) (param $sl i32) (param $vp i32) (param $n i32)
              (param $op i32) (param $oc i32) (result i32)
    (if (i32.ne (local.get $sl) (i32.const 8))
      (then (return (i32.const -1))))
    (if (i32.lt_u (local.get $oc) (i32.const 8))
      (then (return (i32.const -1))))
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $sp)) (i64.extend_i32_u (local.get $n))))
    (i32.const 8))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
    (if (i32.lt_u (local.get $oc) (i32.const 8))
      (then (return (i32.const -1))))
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $ap)) (i64.load (local.get $bp))))
    (i32.const 8))

  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (i32.store (local.get $rp) (i32.const 3))
    (i32.store offset=4 (local.get $rp) (i32.const 0))
    (i64.store offset=8 (local.get $rp) (i64.load (local.get $sp)))
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
