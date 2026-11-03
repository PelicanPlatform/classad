;; aggtrap -- loads and produces an identity, then traps the first time it is
;; asked to fold.
;;
;; The split matters: everything the loader can check passes, so the failure can
;; only arrive mid-query. That is the case the containment rule is about -- an
;; aggregate that stops part-way leaves a state that is not wrong-looking, it is
;; simply too small -- and the engine has to fail the query and name the
;; aggregate rather than report the partial answer.
;;
;; It exports the numeric fold too, so the trap is reachable from the columnar
;; pass and therefore from a WORKER goroutine, where nothing the caller defers
;; can recover it.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (i64.store (local.get $out) (i64.const 0))
    (i32.const 8))

  (func $fold (param i32) (param i32) (param i32) (param i32)
              (param i32) (param i32) (result i32)
    (unreachable))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
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
  (export "classad_udaf_fold_num" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
